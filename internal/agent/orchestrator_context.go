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
	"go-monolith/internal/config"
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
const (
	// maxSystemPromptContextChars caps the dynamic sections of the system
	// prompt: rules, profile facts, retrieved memories and concept chunks. The
	// rolling summary is capped separately by maxSummaryChars and the history by
	// maxContextChars.
	maxSystemPromptContextChars = 12000

	// Per-item caps stop one long passage from crowding out everything else
	// that was retrieved.
	maxRuleChars            = 400
	maxTopMemoryChars       = 400
	maxRetrievedMemoryChars = 800
	maxConceptChunkChars    = 1500

	// maxSourceSnippetChars bounds the text a retrieval hit contributes to the
	// `sources` event, so the SSE payload stays small regardless of chunk size.
	maxSourceSnippetChars = 1500
)

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

// promptBudget distributes a character budget over the dynamic sections of the
// system prompt. Sections are offered in priority order, so the least important
// context is dropped first.
type promptBudget struct {
	remaining int
}

func newPromptBudget(total int) *promptBudget {
	return &promptBudget{remaining: total}
}

// take truncates every item to maxItemChars and returns the longest prefix that
// still fits into the remaining budget. Items are expected to be ordered by
// relevance, so the first one that does not fit ends the section instead of
// being cut down to a fragment that no longer carries meaning.
func (b *promptBudget) take(items []string, maxItemChars int) []string {
	if len(items) == 0 {
		return nil
	}

	kept := make([]string, 0, len(items))
	for _, item := range items {
		item = truncateRunes(item, maxItemChars)
		size := utf8.RuneCountInString(item)
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

func (o *Orchestrator) loadContext(ctx context.Context, userID uuid.UUID, req ChatRequest) (chatContext, []string, error) {
	var chatCtx chatContext
	var memoryIDs []string

	skip := false
	if config.Load().EnableAdaptiveSkip {
		skip = shouldSkipRetrieval(req.Content)
		if skip {
			_ = o.writeTrace(ctx, userID, req.SessionID, "adaptive_skip", map[string]interface{}{
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
			slog.Warn("Failed to search for context", "error", searchErr, "session_id", req.SessionID)
		}
	}

	for _, m := range memories {
		chatCtx.Memories = append(chatCtx.Memories, m.Content)
		memoryIDs = append(memoryIDs, m.ID.String())
		chatCtx.Sources = append(chatCtx.Sources, api.SourceInfo{
			ChunkID:        m.ID,
			FileName:       "Памет (Memory)",
			ContentSnippet: sourceSnippet(m.Content),
			Score:          m.Score * 100,
			ScoreType:      "cosine",
			SourceType:     "memory",
		})
	}

	for _, c := range conceptChunks {
		chatCtx.Chunks = append(chatCtx.Chunks, c.Content)

		chatCtx.Sources = append(chatCtx.Sources, api.SourceInfo{
			ChunkID:        c.ID,
			FileName:       c.FileName,
			ContentSnippet: sourceSnippet(c.Content),
			Score:          c.Score * 100,
			ScoreType:      "cosine",
			SourceType:     "concept",
		})
	}

	// Deduplicate based on ChunkID
	seen := make(map[uuid.UUID]bool)
	var dedupedSources []api.SourceInfo
	for _, s := range chatCtx.Sources {
		if !seen[s.ChunkID] {
			seen[s.ChunkID] = true
			dedupedSources = append(dedupedSources, s)
		}
	}
	chatCtx.Sources = dedupedSources

	// Sort by score descending
	sort.Slice(chatCtx.Sources, func(i, j int) bool {
		return chatCtx.Sources[i].Score > chatCtx.Sources[j].Score
	})

	// Fetch Global Rules. Like retrieval, the rules are an enhancement: a
	// failure here is logged and the chat continues without them instead of
	// failing the whole request.
	rules, err := o.db.Queries.GetGlobalRules(ctx, userID)
	if err != nil {
		slog.Error("failed to get global rules", "err", err, "session_id", req.SessionID)
	} else {
		chatCtx.Rules = rules
	}

	// Fetch Top Memories. Best effort as well, so a profile lookup failure does
	// not abort the generation.
	topMemoriesDb, err := o.db.Queries.GetTopMemories(ctx, db.GetTopMemoriesParams{
		UserID: userID,
		Limit:  15,
	})
	if err != nil {
		slog.Error("failed to get top memories", "err", err, "session_id", req.SessionID)
	}
	for _, m := range topMemoriesDb {
		chatCtx.TopMemories = append(chatCtx.TopMemories, m.Content)
	}

	// Fetch Message History
	dbMsgs, err := o.db.Queries.GetMessagesBySession(ctx, db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Limit:     maxHistoryMessages,
	})
	if err != nil {
		slog.Error("failed to get messages", "err", err)
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
	}

	// Bound the dynamic part of the system prompt. The rules come first because
	// the user configured them explicitly, then the profile facts, then the
	// retrieved memories and finally the concept chunks, which are the most
	// voluminous and therefore the most disposable.
	budget := newPromptBudget(maxSystemPromptContextChars)
	chatCtx.Rules = budget.take(chatCtx.Rules, maxRuleChars)
	chatCtx.TopMemories = budget.take(chatCtx.TopMemories, maxTopMemoryChars)
	chatCtx.Memories = budget.take(chatCtx.Memories, maxRetrievedMemoryChars)
	chatCtx.Chunks = budget.take(chatCtx.Chunks, maxConceptChunkChars)

	return chatCtx, memoryIDs, nil
}
