package agent

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
)

var (
	adaptiveFullRegex = regexp.MustCompile(
		`(?i)^\s*(здрасти|здравей|здравейте|ок|окей|мерси|благодаря|добре\s*благодаря|разбрах|добре|супер|ясно|да|не|` +
			`ok|okay|thanks|thank you|yes|no|got it|hello|hi|hey|nice|cool)\s*[.!?,…]*\s*$`,
	)
	questionWords = []string{"как", "какво", "защо", "кой", "коя", "кое", "кои", "къде",
		"кога", "колко", "може ли", "what", "how", "why", "who", "where", "when", "which", "?"}
)

// The system prompt is assembled from data that is unbounded on the database
// side: book chunks, memories and user defined rules can all be arbitrarily
// long. Without caps a single large chunk pushes the prompt past the model's
// context window, which fails the whole request instead of degrading.
//
// Budgets are charged in estimated tokens rather than characters because the
// content is mostly Bulgarian: a Cyrillic character costs roughly twice what a
// Latin character costs, so a character budget silently lets a Cyrillic prompt
// use twice the context it was meant to.
const (
	// bytesPerToken is the number of bytes one token is assumed to cost.
	// Measured on 2026-11-01 with the Qwen3.5-9B tokenizer: Bulgarian book text
	// 5.06 bytes per token, English 4.49, JSON tool output 3.72. Four is on the
	// safe side for prose (it overestimates the cost by 12-25 %) and about right
	// for JSON; the real usage reported by the server is logged for each request
	// so the ratio can be checked again.
	bytesPerToken = 4

	// Per-item caps stop one long passage from crowding out everything else
	// that was retrieved. They stay in characters so they remain easy to reason
	// about; the section budget itself is charged in estimated tokens.
	maxRuleChars            = 400
	maxTopMemoryChars       = 400
	maxRetrievedMemoryChars = 800
	maxConceptChunkChars    = 1500

	// maxSourceSnippetChars bounds the text a retrieval hit contributes to the
	// `sources` event, so the SSE payload stays small regardless of chunk size.
	maxSourceSnippetChars = 1500
)

// estimateTokens approximates how many model tokens s occupies. It is
// deliberately a heuristic: bundling the model's real tokenizer is not worth it
// for a budget that only needs to be in the right ballpark, but counting bytes
// keeps the estimate comparable across Latin and Cyrillic text.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + bytesPerToken - 1) / bytesPerToken
}

// truncateTokens shortens s so its estimated token cost fits maxTokens. The cut
// is placed on a rune boundary and the ellipsis counts against the budget, so
// the result never costs more than maxTokens even for multibyte text.
func truncateTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if estimateTokens(s) <= maxTokens {
		return s
	}

	// Reserve the ellipsis inside the budget. s is longer than the budget by
	// definition, so the cut always falls inside it.
	cut := maxTokens*bytesPerToken - len("…")
	if cut >= len(s) {
		cut = len(s) - 1
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut <= 0 {
		return "…"
	}
	return s[:cut] + "…"
}

// sourceSnippet renders a retrieval hit for the client: trimmed and capped.
func sourceSnippet(content string) string {
	return truncateRunes(strings.TrimSpace(content), maxSourceSnippetChars)
}

// truncateRunes shortens s to at most max runes, marking the cut with an
// ellipsis so the model does not read a sentence that simply stops.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}

	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

// promptBudget distributes a token budget over the dynamic sections of the
// system prompt. Sections are offered in priority order, so the least important
// context is dropped first.
type promptBudget struct {
	// remaining is the number of estimated tokens still available.
	remaining int
}

func newPromptBudget(total int) *promptBudget {
	return &promptBudget{remaining: total}
}

// take truncates every item to maxItemChars characters and returns the longest
// prefix that still fits into the remaining token budget. Items are expected to
// be ordered by relevance, so the first one that does not fit ends the section
// instead of being cut down to a fragment that no longer carries meaning.
func (b *promptBudget) take(items []string, maxItemChars int) []string {
	if len(items) == 0 {
		return nil
	}

	kept := make([]string, 0, len(items))
	for _, item := range items {
		item = truncateRunes(item, maxItemChars)
		size := estimateTokens(item)
		if size == 0 || size > b.remaining {
			break
		}

		kept = append(kept, item)
		b.remaining -= size
	}
	return kept
}

func shouldSkipRetrieval(query string) bool {
	q := strings.TrimSpace(query)
	if q == "" {
		return true
	}
	if adaptiveFullRegex.MatchString(q) {
		return true
	}
	if len([]rune(q)) < 7 {
		lower := strings.ToLower(q)
		for _, w := range questionWords {
			if strings.Contains(lower, w) {
				return false
			}
		}
		return true
	}
	return false
}

// topMemoryLimit is how many memories, by importance, the prompt always carries as
// profile facts.
const topMemoryLimit = 15

// withoutProfileMemories returns the retrieved memories that are not among the
// profile facts. While the memories are no more than topMemoryLimit, the profile
// holds all of them, and the search for the ones that fit the message returned the
// same four at every message (measured on 2026-11-06: 4 memories, all 4 returned
// for 47 of 47 questions, whatever they were about).
func withoutProfileMemories(retrieved []retrieval.SearchResult, profile []db.Memory) []retrieval.SearchResult {
	if len(retrieved) == 0 || len(profile) == 0 {
		return retrieved
	}
	inProfile := make(map[uuid.UUID]struct{}, len(profile))
	for _, m := range profile {
		inProfile[m.ID] = struct{}{}
	}
	kept := make([]retrieval.SearchResult, 0, len(retrieved))
	for _, r := range retrieved {
		if _, ok := inProfile[r.ID]; !ok {
			kept = append(kept, r)
		}
	}
	return kept
}

func (o *Orchestrator) loadContext(ctx context.Context, userID uuid.UUID, req ChatRequest) (chatContext, []string, error) {
	return o.loadContextWithLimits(ctx, userID, req, o.limits())
}

// loadContextWithLimits is loadContext with the token budgets given: a turn with
// attached files has less room for the history and the retrieved context (see
// planAttachments).
func (o *Orchestrator) loadContextWithLimits(ctx context.Context, userID uuid.UUID, req ChatRequest, lim budgets) (chatContext, []string, error) {
	var chatCtx chatContext
	var memoryIDs []string

	skip := false
	if o.adaptiveSkip {
		skip = shouldSkipRetrieval(req.Content)
		if skip {
			o.trace(ctx, userID, req.SessionID, "adaptive_skip", map[string]interface{}{
				"query": req.Content,
			})
			slog.Info("Adaptive skip triggered for query", "query", req.Content)
		}
	}

	var memories []retrieval.SearchResult
	var conceptChunks []retrieval.SearchResult
	var searchErr error

	if !skip {
		// Call new SearchForContext method
		memories, conceptChunks, searchErr = o.retrieval.SearchForContext(ctx, userID, req.Content, 5)
		// Retrieval is an enhancement, not a requirement: a degraded search must
		// not cost the user their answer, so the error is logged and the chat
		// continues without retrieved context.
		if searchErr != nil {
			slog.WarnContext(ctx, "Failed to search for context", "error", searchErr, "session_id", req.SessionID)
		}
	}

	// Fetch Top Memories. Best effort as well, so a profile lookup failure does
	// not abort the generation. They are fetched before the retrieved memories are
	// used because the prompt carries them at every message, and a retrieved memory
	// that is one of them would only say the same thing twice.
	topMemoriesDb, err := o.db.Queries.GetTopMemories(ctx, db.GetTopMemoriesParams{
		UserID: userID,
		Limit:  topMemoryLimit,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to get top memories", "err", err, "session_id", req.SessionID)
	}
	for _, m := range topMemoriesDb {
		chatCtx.TopMemories = append(chatCtx.TopMemories, m.Content)
	}
	memories = withoutProfileMemories(memories, topMemoriesDb)

	for _, m := range memories {
		chatCtx.Memories = append(chatCtx.Memories, m.Content)
		memoryIDs = append(memoryIDs, m.ID.String())
	}

	for _, c := range conceptChunks {
		chatCtx.Chunks = append(chatCtx.Chunks, c.Content)
	}

	// Fetch Global Rules. Like retrieval, the rules are an enhancement: a
	// failure here is logged and the chat continues without them instead of
	// failing the whole request.
	rules, err := o.db.Queries.GetGlobalRules(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get global rules", "err", err, "session_id", req.SessionID)
	} else {
		chatCtx.Rules = rules
	}

	// Fetch Message History
	dbMsgs, err := o.db.Queries.GetMessagesBySession(ctx, db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Limit:     int32(lim.historyMessages),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to get messages", "err", err)
		return chatCtx, memoryIDs, err
	}

	// dbMsgs are returned in DESC order (newest first).
	// We need them in ASC order (oldest first) for the prompt history.
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		m := dbMsgs[i]
		msg := openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		if msg.Role == "assistant" && msg.Content == "" && len(msg.ToolCalls) == 0 {
			continue
		}
		chatCtx.History = append(chatCtx.History, msg)
		chatCtx.HistoryAt = append(chatCtx.HistoryAt, m.CreatedAt.Time)
	}

	// Bound the dynamic part of the system prompt. The rules come first because
	// the user configured them explicitly, then the profile facts, then the
	// retrieved memories and finally the concept chunks, which are the most
	// voluminous and therefore the most disposable.
	budget := newPromptBudget(lim.promptContextTokens)
	chatCtx.Rules = budget.take(chatCtx.Rules, maxRuleChars)
	chatCtx.TopMemories = budget.take(chatCtx.TopMemories, maxTopMemoryChars)
	chatCtx.Memories = budget.take(chatCtx.Memories, maxRetrievedMemoryChars)
	chatCtx.Chunks = budget.take(chatCtx.Chunks, maxConceptChunkChars)

	// The panel lists what the model was given. take keeps a prefix, so the first
	// len(kept) memories and summaries are the ones in the prompt; a source the
	// budget cut would be shown as read although the model never saw it.
	chatCtx.Sources = contextSources(memories[:len(chatCtx.Memories)], conceptChunks[:len(chatCtx.Chunks)])

	return chatCtx, memoryIDs, nil
}

// passageSource is a passage of a book that the search_documents tool found, as the
// sources panel shows it: the same "text" or "ocr" type the history gives it, and no
// score - the hybrid search ranks the passages but measures none of them.
func passageSource(res retrieval.SearchResult) api.SourceInfo {
	sourceType := "text"
	content := res.Content
	if strings.Contains(content, api.OCRMarker) {
		sourceType = "ocr"
		content = strings.ReplaceAll(content, api.OCRMarker, "")
	}
	return api.SourceInfo{
		ChunkID:        res.ID,
		FileName:       res.FileName,
		ContentSnippet: sourceSnippet(content),
		SourceType:     sourceType,
	}
}

// contextSources are the sources of the prompt for the memories and concept
// summaries it carries: the closest summary first, then the memories in the order
// they were found, each chunk once.
func contextSources(memories, concepts []retrieval.SearchResult) []api.SourceInfo {
	var sources []api.SourceInfo
	for _, m := range memories {
		sources = append(sources, api.SourceInfo{
			ChunkID:        m.ID,
			FileName:       "Памет (Memory)",
			ContentSnippet: sourceSnippet(m.Content),
			// A recalled memory has no distance to show: its score is a blend of
			// the vector and the text match and of its importance.
			SourceType: "memory",
		})
	}
	for _, c := range concepts {
		sources = append(sources, api.SourceInfo{
			ChunkID:        c.ID,
			FileName:       c.FileName,
			ContentSnippet: sourceSnippet(c.Content),
			Score:          api.RoundDistance(c.Distance),
			ScoreType:      api.ScoreTypeDistance,
			SourceType:     "concept",
		})
	}

	sources = withoutKnownSources(nil, sources)
	sort.SliceStable(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		aMeasured, bMeasured := a.ScoreType == api.ScoreTypeDistance, b.ScoreType == api.ScoreTypeDistance
		if aMeasured != bMeasured {
			return aMeasured
		}
		return aMeasured && a.Score < b.Score
	})
	return sources
}

// withoutKnownSources returns the sources of extra whose chunk is not in known,
// and not twice among themselves, in their order.
func withoutKnownSources(known, extra []api.SourceInfo) []api.SourceInfo {
	seen := make(map[uuid.UUID]bool, len(known)+len(extra))
	for _, s := range known {
		seen[s.ChunkID] = true
	}
	var fresh []api.SourceInfo
	for _, s := range extra {
		if !seen[s.ChunkID] {
			seen[s.ChunkID] = true
			fresh = append(fresh, s)
		}
	}
	return fresh
}
