package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/proposals"
	"go-monolith/internal/retrieval"
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
	case "save_global_rule":
		return r.executeSaveGlobalRule(ctx, userID, args)
	case "web_search":
		return r.executeWebSearch(ctx, args)
	case "web_fetch":
		return r.executeWebFetch(ctx, args)
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

// maxToolSearchResults is the most results one search tool call returns, so the
// answer fits the model's context. It is also the default, and the schema in
// toolDefinitions tells the model so.
const maxToolSearchResults = 5

// searchLimitArg reads the optional "limit" of a search tool, capped at
// maxToolSearchResults.
func searchLimitArg(args map[string]interface{}) int {
	return min(intArg(args, "limit", maxToolSearchResults), maxToolSearchResults)
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

func (r *DefaultToolRegistry) executeWebSearch(ctx context.Context, args map[string]interface{}) (interface{}, error) {
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
	turnFrom(ctx).markExternal(sourceWebSearch)
	return result, nil
}

func (r *DefaultToolRegistry) executeWebFetch(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	rawURL, ok := stringArg(args, "url")
	if !ok {
		return nil, missingArgError("web_fetch", "url", `{"url": "<page url>"}`)
	}
	pageURL, err := validateFetchURL(rawURL)
	if err != nil {
		metrics.WebToolRequests.WithLabelValues("web_fetch", "rejected").Inc()
		slog.Warn("web_fetch refused a URL", "err", err, "url", truncateRunes(rawURL, maxLoggedToolArgsRunes))
		return nil, &toolArgumentError{tool: "web_fetch", reason: err.Error()}
	}

	result, err := r.callWebTool(ctx, "web_fetch", "/fetch", map[string]interface{}{"url": pageURL})
	if err != nil {
		return nil, err
	}
	turnFrom(ctx).markExternal(sourceWebFetch)
	return result, nil
}

// pendingApprovalResult is what the model is told when a write was turned into a
// proposal: it must not claim the thing is saved.
func pendingApprovalResult(p proposals.Proposal) map[string]interface{} {
	return map[string]interface{}{
		"status":      "pending_approval",
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
			"status":  "skipped",
			"reason":  "query is conversational filler",
			"results": []interface{}{},
		}, nil
	}

	limit := searchLimitArg(args)

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

func (r *DefaultToolRegistry) executeSearchDocuments(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	query, err := searchQueryArg(ctx, "search_documents", args)
	if err != nil {
		return nil, err
	}

	if shouldSkipRetrieval(query) {
		return map[string]interface{}{
			"status":  "skipped",
			"reason":  "query is conversational filler",
			"results": []interface{}{},
		}, nil
	}

	limit := searchLimitArg(args)

	results, err := r.retrieval.SearchRawChunks(ctx, userID, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search documents: %w", err)
	}
	if len(results) > 0 {
		turnFrom(ctx).markExternal(sourceSearchDocuments)
	}

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

	return map[string]interface{}{
		"status":  "success",
		"results": formattedResults,
	}, nil
}
