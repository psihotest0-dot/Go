package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
)

// DefaultToolRegistry is the concrete implementation of ToolRegistry.
type DefaultToolRegistry struct {
	retrieval *retrieval.RetrievalService
	db        *db.Database
}

// NewDefaultToolRegistry creates a new DefaultToolRegistry.
func NewDefaultToolRegistry(r *retrieval.RetrievalService, d *db.Database) *DefaultToolRegistry {
	return &DefaultToolRegistry{
		retrieval: r,
		db:        d,
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
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
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

func (r *DefaultToolRegistry) executeSearchMemories(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("invalid or missing 'query' argument")
	}

	limit := 5
	if lim, ok := args["limit"].(float64); ok {
		limit = int(lim)
	}
	if limit > 20 {
		limit = 20
	}

	results, err := r.retrieval.SearchMemories(ctx, userID, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search memories: %w", err)
	}

	var formattedResults []map[string]interface{}
	for _, res := range results {
		formattedResults = append(formattedResults, map[string]interface{}{
			"id":      res.ID,
			"content": res.Content,
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

	limit := 5
	if lim, ok := args["limit"].(float64); ok {
		limit = int(lim)
	}
	if limit > 20 {
		limit = 20
	}

	results, err := r.retrieval.SearchChunks(ctx, userID, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search documents: %w", err)
	}

	var formattedResults []map[string]interface{}
	for _, res := range results {
		formattedResults = append(formattedResults, map[string]interface{}{
			"id":        res.ID,
			"content":   res.Content,
			"score":     res.Score,
			"file_name": res.FileName,
		})
	}

	return map[string]interface{}{
		"status":  "success",
		"results": formattedResults,
	}, nil
}
