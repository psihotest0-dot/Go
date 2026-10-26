package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
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

func (r *DefaultToolRegistry) executeWebSearch(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("invalid or missing 'query' argument")
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
	url, ok := args["url"].(string)
	if !ok || url == "" {
		return nil, fmt.Errorf("invalid or missing 'url' argument")
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
	key, ok := args["key"].(string)
	if !ok || key == "" {
		return nil, fmt.Errorf("invalid or missing 'key' argument")
	}

	value, ok := args["value"].(string)
	if !ok || value == "" {
		return nil, fmt.Errorf("invalid or missing 'value' argument")
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
	content, ok := args["content"].(string)
	if !ok || content == "" {
		return nil, fmt.Errorf("invalid or missing 'content' argument")
	}

	importance := 5
	if imp, ok := args["importance"].(float64); ok {
		importance = int(imp)
	}

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
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("invalid or missing 'query' argument")
	}

	if shouldSkipRetrieval(query) {
		return map[string]interface{}{
			"status":  "skipped",
			"reason":  "query is conversational filler",
			"results": []interface{}{},
		}, nil
	}

	limit := 5
	if lim, ok := args["limit"].(float64); ok {
		limit = int(lim)
	}
	// Enforce hard cap of 5 chunks for LLM context limits
	if limit > 5 {
		limit = 5
	}

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
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("invalid or missing 'query' argument")
	}

	if shouldSkipRetrieval(query) {
		return map[string]interface{}{
			"status":  "skipped",
			"reason":  "query is conversational filler",
			"results": []interface{}{},
		}, nil
	}

	limit := 5
	if lim, ok := args["limit"].(float64); ok {
		limit = int(lim)
	}
	// Enforce hard cap of 5 chunks for LLM context limits
	if limit > 5 {
		limit = 5
	}

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
