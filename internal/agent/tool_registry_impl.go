package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/attachments"
	"go-monolith/internal/db"
	"go-monolith/internal/proposals"
	"go-monolith/internal/retrieval"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// DefaultToolRegistry is the concrete implementation of ToolRegistry.
type DefaultToolRegistry struct {
	retrieval *retrieval.RetrievalService
	db        *db.Database
	// proposals holds the writes that wait for the user (global rules, and memories
	// of a turn that read untrusted text).
	proposals  *proposals.Service
	httpClient *http.Client
	// webBaseURL replaces the paid web API in tests; empty means the real one.
	webBaseURL string
	// attachments holds the files attached to a conversation; nil means none can be.
	attachments *attachments.Service
	// files is the folder of the user's own files; nil means the file tools are off.
	files *userfiles.Store
	// fetcher and adder are what add_to_library works with; nil means the tool is off.
	fetcher documentFetcher
	adder   libraryAdder
	// plans keeps the plans of the tasks; nil means the plan tools are off.
	plans *taskplan.Store
	// pages fetches web_fetch's pages from this machine; nil means only the paid API (web_fetch_local.go).
	pages pageFetcher
	// wayback lets web_fetch, save_page and add_to_library read a link from its copy in the
	// Wayback Machine (wayback.go).
	wayback bool
}

// WithAttachments lets the read_attachment tool read the files attached to a
// conversation.
func (r *DefaultToolRegistry) WithAttachments(s *attachments.Service) *DefaultToolRegistry {
	r.attachments = s
	return r
}

// NewDefaultToolRegistry creates a new DefaultToolRegistry.
func NewDefaultToolRegistry(r *retrieval.RetrievalService, d *db.Database, p *proposals.Service) *DefaultToolRegistry {
	return &DefaultToolRegistry{
		retrieval:  r,
		db:         d,
		proposals:  p,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// ExecuteTool implements the ToolRegistry interface.
func (r *DefaultToolRegistry) ExecuteTool(ctx context.Context, name string, args map[string]interface{}) (interface{}, error) {
	userID, ok := ctx.Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		return nil, fmt.Errorf("unauthorized: missing user ID in context")
	}

	switch name {
	case "create_memory":
		return r.executeCreateMemory(ctx, userID, args)
	case "search_memories":
		return r.executeSearchMemories(ctx, userID, args)
	case "search_documents", "search_knowledge_base":
		return r.executeSearchDocuments(ctx, userID, args)
	case listDocumentsTool:
		return r.executeListDocuments(ctx, userID)
	case readAttachmentTool:
		return r.executeReadAttachment(ctx, userID, args)
	case addToLibraryTool:
		return r.executeAddToLibrary(ctx, userID, args)
	case planStubTool, planSetTool, planUpdateTool:
		if r.plans == nil {
			return nil, fmt.Errorf("unknown tool: %s", name)
		}
		switch name {
		case planStubTool:
			return r.executePlanStub()
		case planSetTool:
			return r.executePlanSet(ctx, userID, args)
		default:
			return r.executePlanUpdate(ctx, userID, args)
		}
	case "save_global_rule":
		return r.executeSaveGlobalRule(ctx, userID, args)
	case styleNoteTool:
		return r.executeSaveStyleNote(ctx, userID, args)
	case "web_search":
		return r.executeWebSearch(ctx, args)
	case "web_fetch":
		return r.executeWebFetch(ctx, args)
	case filesStubTool, listFilesTool, readFileTool, writeFileTool, appendFileTool, savePageTool:
		if r.files == nil {
			return nil, fmt.Errorf("unknown tool: %s", name)
		}
		switch name {
		case savePageTool:
			return r.executeSavePage(ctx, userID, args)
		case filesStubTool:
			return r.executeFilesUnlock()
		case listFilesTool:
			return r.executeListFiles(userID)
		case readFileTool:
			return r.executeReadFile(ctx, userID, args)
		case writeFileTool:
			return r.executeWriteFile(ctx, userID, args)
		default:
			return r.executeAppendFile(ctx, userID, args)
		}
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// toolArgumentError reports a tool call the model got wrong: a required
// argument is missing, empty or of the wrong type. The server is healthy, so
// the orchestrator logs it as a warning and passes the message back to the
// model, which can retry the call with the arguments its schema requires.
type toolArgumentError struct {
	tool   string
	reason string
}

func (e *toolArgumentError) Error() string {
	return fmt.Sprintf("invalid arguments for %s: %s", e.tool, e.reason)
}

// missingArgError builds the model facing error for an absent argument. The
// example shows the shape the model should retry with.
func missingArgError(tool, arg, example string) error {
	reason := fmt.Sprintf("missing required %q argument", arg)
	if example != "" {
		reason += "; call the tool again with " + example
	}
	return &toolArgumentError{tool: tool, reason: reason}
}

// stringArg reads a required string argument and trims it, so a value made of
// whitespace counts as missing instead of reaching the database.
func stringArg(args map[string]interface{}, name string) (string, bool) {
	raw, ok := args[name]
	if !ok {
		return "", false
	}
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

// intArg reads an optional positive integer argument. JSON decoding produces
// float64, but json.Number and numeric strings are accepted too because some
// model servers emit numbers that way.
func intArg(args map[string]interface{}, name string, fallback int) int {
	switch value := args[name].(type) {
	case float64:
		if value > 0 {
			return int(value)
		}
	case json.Number:
		if n, err := value.Int64(); err == nil && n > 0 {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// The most results one call of a search tool returns, so the answer fits the model's
// context. Each is also the default of its tool, and the schema in toolDefinitions
// tells the model so.
//
// Documents get 8: on 33 questions with a known answer, the right book was among the
// results 26 times with 5 and 28 with 8 (a relevant passage 20 times against 22), at
// about 1 000 more tokens per call - the tool results may use 4 000 per round.
// Memories stay at 5; nothing was measured for them.
const (
	maxMemorySearchResults   = 5
	maxDocumentSearchResults = 8
)

// searchLimitArg reads the optional "limit" of a search tool, capped at maxResults.
func searchLimitArg(args map[string]interface{}, maxResults int) int {
	return min(intArg(args, "limit", maxResults), maxResults)
}

// maxFallbackQueryRunes bounds the user message reused as a search query. The
// message can be a whole paragraph, while the embedding model expects a short
// query.
const maxFallbackQueryRunes = 500

// searchQueryArg resolves the text a search tool must run. Models regularly
// call search_memories or search_documents with an empty argument object; in
// that case the user message that triggered the turn is a better query than
// failing the turn with an error the model cannot explain to the user.
func searchQueryArg(ctx context.Context, tool string, args map[string]interface{}) (string, error) {
	if query, ok := stringArg(args, "query"); ok {
		return query, nil
	}

	if query := strings.TrimSpace(currentQuery(ctx)); query != "" {
		query = truncateRunes(query, maxFallbackQueryRunes)
		slog.Warn("search tool called without a query, using the current user message",
			"tool", tool, "query_chars", utf8.RuneCountInString(query))
		return query, nil
	}

	return "", missingArgError(tool, "query", `{"query": "<what to search for>"}`)
}

// webAfterFilesError refuses a web tool in a turn that read a file of the user's folder:
// text from a page or a book could ask the model to send what it read out through the
// query or the address (plan item P44).
func webAfterFilesError(ctx context.Context, tool string) error {
	if !turnFrom(ctx).has(sourceUserFiles) {
		return nil
	}
	metrics.WebToolRequests.WithLabelValues(tool, "rejected").Inc()
	return &toolArgumentError{tool: tool, reason: "the web cannot be used in a message where the user's files were read; " +
		"tell the user to ask for the web search in a separate message"}
}

func (r *DefaultToolRegistry) executeWebSearch(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	if err := webAfterFilesError(ctx, "web_search"); err != nil {
		return nil, err
	}
	query, ok := stringArg(args, "query")
	if !ok {
		return nil, missingArgError("web_search", "query", `{"query": "<what to search for>"}`)
	}
	if err := webSearchQuery(query); err != nil {
		metrics.WebToolRequests.WithLabelValues("web_search", "rejected").Inc()
		return nil, err
	}

	result, err := r.callWebTool(ctx, "web_search", "/search", map[string]interface{}{"query": query})
	if err != nil {
		return nil, err
	}
	if answer, ok := result["result"].(map[string]interface{}); ok {
		result["result"] = trimSearchResult(answer)
	}
	turnFrom(ctx).markExternal(sourceWebSearch)
	return result, nil
}

func (r *DefaultToolRegistry) executeWebFetch(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	if err := webAfterFilesError(ctx, "web_fetch"); err != nil {
		return nil, err
	}
	rawURL, ok := stringArg(args, "url")
	if !ok {
		return nil, missingArgError("web_fetch", "url", `{"url": "<page url>"}`)
	}
	pageURL, err := validateFetchURL(rawURL)
	if err != nil {
		metrics.WebToolRequests.WithLabelValues("web_fetch", "rejected").Inc()
		// A link is logged by its host (the code review of 2026-11-27: it was logged whole).
		slog.WarnContext(ctx, "web_fetch refused a URL", "err", err, "host", hostOf(rawURL))
		return nil, &toolArgumentError{tool: "web_fetch", reason: err.Error()}
	}

	// From this machine first (web_fetch_local.go); the paid API when that gives nothing.
	localReason := ""
	if r.pages != nil {
		var result map[string]interface{}
		if result, localReason = r.fetchLocally(ctx, pageURL); result != nil {
			turnFrom(ctx).markExternal(sourceWebFetch)
			return result, nil
		}
	}

	// A page gone for this machine (404) is gone for the paid API too: its copy comes first (wayback.go).
	archiveTried := localReason == string(webfetch.ReasonNotFound)
	if archiveTried {
		if archived := r.archivedFetchResult(ctx, pageURL, localReason); archived != nil {
			turnFrom(ctx).markExternal(sourceWebFetch)
			return archived, nil
		}
	}

	result, err := r.callWebTool(ctx, "web_fetch", "/fetch", map[string]interface{}{"url": pageURL})
	if err != nil {
		if !archiveTried {
			if archived := r.archivedFetchResult(ctx, pageURL, localReason); archived != nil {
				turnFrom(ctx).markExternal(sourceWebFetch)
				return archived, nil
			}
		}
		if localReason != "" {
			// The page could be read neither here nor by the API: the model hears why from here.
			slog.WarnContext(ctx, "the paid web_fetch failed after the local one", "error", err)
			return nil, localFetchError(localReason)
		}
		return nil, err
	}
	turnFrom(ctx).markExternal(sourceWebFetch)
	return result, nil
}

// The statuses of the tool results that the server reads back (the record of the tools,
// tool_log.go, and the evidence of a plan, plan_tool.go): a word changed where a result is
// made must change where it is read.
const (
	statusNotFound        = "not_found"
	statusSaved           = "saved"
	statusAppended        = "appended"
	statusPendingApproval = "pending_approval"
	statusSkipped         = "skipped"
)

// pendingApprovalResult is what the model is told when a write was turned into a
// proposal: it must not claim the thing is saved.
func pendingApprovalResult(p proposals.Proposal) map[string]interface{} {
	return map[string]interface{}{
		"status":      statusPendingApproval,
		"proposal_id": p.ID,
		"proposal":    p.Summary,
		"message": "NOT saved yet. This is only a proposal and it waits for the user to confirm it. " +
			"Tell the user briefly what you propose to save and that they can confirm or reject it. " +
			"Do not say that it is saved.",
	}
}

// proposalError turns the errors of the proposals package into what the model
// sees: a call it can fix or an expected refusal, not a server failure.
func proposalError(tool string, err error) error {
	var invalid *proposals.InvalidError
	switch {
	case errors.As(err, &invalid):
		return &toolArgumentError{tool: tool, reason: invalid.Reason}
	case errors.Is(err, proposals.ErrTooManyPending):
		return &toolArgumentError{tool: tool, reason: fmt.Sprintf(
			"the user already has %d proposals waiting for an answer; ask them to confirm or reject those first", proposals.MaxPending)}
	case errors.Is(err, proposals.ErrTooManyRules):
		return &toolArgumentError{tool: tool, reason: fmt.Sprintf(
			"the user already has %d global rules; ask them to delete one before adding another", proposals.MaxActiveRules)}
	case errors.Is(err, proposals.ErrTooManyStyleNotes):
		return &toolArgumentError{tool: tool, reason: fmt.Sprintf(
			"the user already has %d notes on how to talk; ask them to delete one before adding another", proposals.MaxStyleNotes)}
	}
	return fmt.Errorf("failed to propose: %w", err)
}

// executeSaveGlobalRule never writes the rule. A rule is the first thing in the
// system prompt of every session, so it only becomes one when the user confirms.
func (r *DefaultToolRegistry) executeSaveGlobalRule(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	key, ok := stringArg(args, "key")
	if !ok {
		return nil, missingArgError("save_global_rule", "key", `{"key": "<short rule name>", "value": "<instruction>"}`)
	}

	value, ok := stringArg(args, "value")
	if !ok {
		return nil, missingArgError("save_global_rule", "value", `{"key": "<short rule name>", "value": "<instruction>"}`)
	}

	turn := turnFrom(ctx)
	p, err := r.proposals.ProposeRule(ctx, userID, turn.session(), key, value, turn.externalSources())
	if err != nil {
		return nil, proposalError("save_global_rule", err)
	}
	turn.addProposal(p)
	return pendingApprovalResult(p), nil
}

// styleNoteTool proposes a note on how to talk with the user (plan item P42).
const styleNoteTool = "save_style_note"

// executeSaveStyleNote never writes the note either: like a rule it is in every
// prompt, so it only becomes one when the user confirms.
func (r *DefaultToolRegistry) executeSaveStyleNote(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	const example = `{"key": "<short name>", "value": "<how he likes to be talked to>"}`
	key, ok := stringArg(args, "key")
	if !ok {
		return nil, missingArgError(styleNoteTool, "key", example)
	}
	value, ok := stringArg(args, "value")
	if !ok {
		return nil, missingArgError(styleNoteTool, "value", example)
	}

	turn := turnFrom(ctx)
	p, err := r.proposals.ProposeStyleNote(ctx, userID, turn.session(), key, value, turn.externalSources())
	if err != nil {
		return nil, proposalError(styleNoteTool, err)
	}
	turn.addProposal(p)
	return pendingApprovalResult(p), nil
}

// executeCreateMemory saves the fact. What to remember is the model's decision,
// as it is for a person in a conversation; unlike a global rule it is not held
// for the user's confirmation. When the turn had read a web page or a document,
// the memory is marked with where that text came from, so a doubtful one can be
// found and removed later.
func (r *DefaultToolRegistry) executeCreateMemory(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	content, ok := stringArg(args, "content")
	if !ok {
		return nil, missingArgError("create_memory", "content", `{"content": "<fact about the user>"}`)
	}

	importance, err := importanceArg(args)
	if err != nil {
		return nil, err
	}
	memory, err := proposals.NormalizeMemory(content, importance)
	if err != nil {
		return nil, proposalError("create_memory", err)
	}

	var metadata pqtype.NullRawMessage
	if external := turnFrom(ctx).externalSources(); len(external) > 0 {
		raw, _ := json.Marshal(map[string]any{"source": "chat_tool", "external_sources": external})
		metadata = pqtype.NullRawMessage{RawMessage: raw, Valid: true}
	}

	// Always save as "active" to avoid conflicts, letting the background worker consolidate.
	mem, err := r.retrieval.CreateMemory(ctx, userID, memory.Content, memory.Importance, "active", metadata)
	if err != nil {
		if errors.Is(err, db.ErrMemoryExists) {
			// If it's the exact same content, we can just return success
			return map[string]interface{}{
				"status":    "success",
				"message":   "Memory already exists",
				"memory_id": mem.ID, // CreateMemoryWithCheck returns existing on ErrMemoryExists
				"content":   mem.Content,
			}, nil
		}
		return nil, fmt.Errorf("failed to create memory: %w", err)
	}

	return map[string]interface{}{
		"status":    "success",
		"memory_id": mem.ID,
		"content":   mem.Content,
	}, nil
}

// importanceArg reads the optional importance of create_memory. Absent means the
// default (0); a value outside 1-10 is an error the model can correct.
func importanceArg(args map[string]interface{}) (int, error) {
	raw, present := args["importance"]
	if !present || raw == nil {
		return 0, nil
	}

	notWhole := &toolArgumentError{tool: "create_memory", reason: "importance must be a whole number from 1 to 10"}
	var n int
	switch value := raw.(type) {
	case float64:
		n = int(value)
		if float64(n) != value {
			return 0, notWhole
		}
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, notWhole
		}
		n = int(parsed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, notWhole
		}
		n = parsed
	default:
		return 0, notWhole
	}

	if n < proposals.MinImportance || n > proposals.MaxImportance {
		return 0, &toolArgumentError{tool: "create_memory", reason: fmt.Sprintf(
			"importance must be between %d and %d", proposals.MinImportance, proposals.MaxImportance)}
	}
	return n, nil
}

func truncateString(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
}

func (r *DefaultToolRegistry) executeSearchMemories(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	query, err := searchQueryArg(ctx, "search_memories", args)
	if err != nil {
		return nil, err
	}

	if shouldSkipRetrieval(query) {
		return map[string]interface{}{
			"status":  statusSkipped,
			"reason":  "query is conversational filler",
			"results": []interface{}{},
		}, nil
	}

	limit := searchLimitArg(args, maxMemorySearchResults)

	results, err := r.retrieval.SearchMemories(ctx, userID, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search memories: %w", err)
	}

	var formattedResults []map[string]interface{}
	for _, res := range results {
		content := truncateString(res.Content, 1500)
		formattedResults = append(formattedResults, map[string]interface{}{
			"id":      res.ID,
			"content": content,
			"score":   res.Score,
		})
	}

	return map[string]interface{}{
		"status":  "success",
		"results": formattedResults,
	}, nil
}

const (
	// queryENArg is the optional argument of search_documents that carries the query
	// in English; maxQueryENRunes bounds it, as it comes from the model.
	queryENArg      = "query_en"
	maxQueryENRunes = 300
)

// mergePassages joins the results of two searches of one question, best score first.
// A passage found by both is listed once, with its higher score; at most limit remain.
// Scores of the two searches compare: both are 1 - cosine distance of the passage to
// its query (or the lower score of a text match), so a Bulgarian book found by the
// Bulgarian query and an English one found by the English query stand side by side.
func mergePassages(limit int, lists ...[]retrieval.SearchResult) []retrieval.SearchResult {
	var merged []retrieval.SearchResult
	at := map[uuid.UUID]int{}
	for _, list := range lists {
		for _, res := range list {
			if i, seen := at[res.ID]; seen {
				if res.Score > merged[i].Score {
					merged[i].Score = res.Score
				}
				continue
			}
			at[res.ID] = len(merged)
			merged = append(merged, res)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func (r *DefaultToolRegistry) executeSearchDocuments(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	query, err := searchQueryArg(ctx, "search_documents", args)
	if err != nil {
		return nil, err
	}
	queryEN, _ := stringArg(args, queryENArg)
	// A search about programming in a conversation with attached code gets the language of the
	// code (code_language.go): the library has books on many languages.
	if language := turnFrom(ctx).attachedLanguage(); language != "" {
		query, queryEN = withCodeLanguage(query, language), withCodeLanguage(queryEN, language)
	}

	// A question that names one file of the library ("какво пише във файла CLBSV2") is
	// about that file: the search stays inside it. The search of the whole library
	// keeps at most two passages of a file and fills the answer with the nearest ones of
	// other books, which then also showed up as sources (2026-11-15: 2 passages of the
	// file, 6 of Freud, a cookbook and a book about vitamin B12).
	scope, scoped := r.retrieval.FileNamedIn(ctx, userID, currentQuery(ctx), query)

	// The first search of an explanation from the library is the dossier of the code (explain.go);
	// a question that names a file stays inside the file, as any search does.
	if subject := turnFrom(ctx).takeExplain(); subject != "" {
		if !scoped {
			return r.explainDossier(ctx, userID, query, queryEN, subject)
		}
		metrics.ExplainDossiers.WithLabelValues(explainScoped).Inc()
	}

	// A short query is taken for filler ("да", "hi"), and so was "CLBSV2" - the name of the
	// file itself, which the model sends when it is asked what is in the file (measured on
	// 2026-11-15). When the question names a file, the query is the file and is searched.
	if !scoped && shouldSkipRetrieval(query) {
		return map[string]interface{}{
			"status":  statusSkipped,
			"reason":  "query is conversational filler",
			"results": []interface{}{},
		}, nil
	}

	limit := searchLimitArg(args, maxDocumentSearchResults)

	search := func(q string) ([]retrieval.SearchResult, error) {
		if scoped {
			return r.retrieval.SearchFileChunks(ctx, userID, scope.ID, q, limit)
		}
		return r.retrieval.SearchRawChunks(ctx, userID, q, limit)
	}

	results, err := search(query)
	if err != nil {
		return nil, fmt.Errorf("failed to search documents: %w", err)
	}
	// The same question in English, when the model supplied it: many books are in
	// English and a Bulgarian query often misses them (task 32, 2026-11-08). The
	// first search stands on its own, so a failure of the second one costs nothing.
	if queryEN != "" && !strings.EqualFold(queryEN, query) {
		second, err := search(truncateRunes(queryEN, maxQueryENRunes))
		if err != nil {
			slog.WarnContext(ctx, "the English search of search_documents failed, using the first one", "error", err)
		} else {
			results = mergePassages(limit, results, second)
		}
	}
	results, earlier := r.withEarlierAnswer(ctx, userID, query, scoped, limit, results)
	if len(results) > 0 || len(earlier) > 0 {
		turnFrom(ctx).markExternal(sourceSearchDocuments)
	}

	// The model reads these passages, so the sources panel must list them too.
	found := make([]api.SourceInfo, 0, len(results))
	for _, res := range results {
		found = append(found, passageSource(res))
	}
	turnFrom(ctx).addSources(found)

	var formattedResults []map[string]interface{}
	for _, res := range results {
		content := truncateString(res.Content, 1500)
		formattedResults = append(formattedResults, map[string]interface{}{
			"id":        res.ID,
			"content":   content,
			"score":     res.Score,
			"file_name": res.FileName,
		})
	}

	// The passages of the earlier answer: for the model to know what "it" is and not to tell
	// them again as new; not in the sources panel, they are not a source of this answer.
	for _, p := range earlier {
		formattedResults = append(formattedResults, map[string]interface{}{
			"id":                  p.ID,
			"content":             truncateString(p.Content, 1500),
			"file_name":           p.FileName,
			"from_earlier_answer": true,
		})
	}
	out := map[string]interface{}{
		"status":  "success",
		"results": formattedResults,
	}
	if len(earlier) > 0 {
		out["note"] = earlierPassagesNote
	}
	if scoped {
		// The model should know that these are the passages of that file only, not the
		// best ones of the library.
		out["searched_only_in_file"] = scope.Name
	}
	return out, nil
}
