package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
)

// DefaultToolRegistry is the concrete implementation of ToolRegistry.
type DefaultToolRegistry struct {
	retrieval  *retrieval.RetrievalService
	db         *db.Database
	httpClient *http.Client
}

// NewDefaultToolRegistry creates a new DefaultToolRegistry.
func NewDefaultToolRegistry(r *retrieval.RetrievalService, d *db.Database) *DefaultToolRegistry {
	return &DefaultToolRegistry{
		retrieval:  r,
		db:         d,
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

	apiKey := os.Getenv("DEEPSEEK_API_KEY")

	reqBody, _ := json.Marshal(map[string]interface{}{"query": query})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.deepseek.com/v1/tools/search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform search request: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search API returned status %d: %v", resp.StatusCode, result)
	}

	return result, nil
}

func (r *DefaultToolRegistry) executeWebFetch(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	url, ok := stringArg(args, "url")
	if !ok {
		return nil, missingArgError("web_fetch", "url", `{"url": "<page url>"}`)
	}

	apiKey := os.Getenv("DEEPSEEK_API_KEY")

	reqBody, _ := json.Marshal(map[string]interface{}{"url": url})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.deepseek.com/v1/tools/fetch", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform fetch request: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch API returned status %d: %v", resp.StatusCode, result)
	}

	return result, nil
}

func (r *DefaultToolRegistry) executeSaveGlobalRule(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	key, ok := stringArg(args, "key")
	if !ok {
		return nil, missingArgError("save_global_rule", "key", `{"key": "<short rule name>", "value": "<instruction>"}`)
	}

	value, ok := stringArg(args, "value")
	if !ok {
		return nil, missingArgError("save_global_rule", "value", `{"key": "<short rule name>", "value": "<instruction>"}`)
	}

	err := r.db.Queries.SaveGlobalRule(ctx, db.SaveGlobalRuleParams{
		UserID: userID,
		Key:    key,
		Value:  value,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save global rule: %w", err)
	}

	return map[string]interface{}{
		"status": "success",
		"key":    key,
		"value":  value,
	}, nil
}

func (r *DefaultToolRegistry) executeCreateMemory(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	content, ok := stringArg(args, "content")
	if !ok {
		return nil, missingArgError("create_memory", "content", `{"content": "<fact about the user>"}`)
	}

	importance := intArg(args, "importance", 5)

	// Always save as "active" to avoid conflicts, letting the background worker consolidate.
	mem, err := r.retrieval.CreateMemory(ctx, userID, content, importance, "active", pqtype.NullRawMessage{})
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
