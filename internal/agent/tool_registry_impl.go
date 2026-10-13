package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/retrieval"
)

// DefaultToolRegistry is the concrete implementation of ToolRegistry.
type DefaultToolRegistry struct {
	retrieval *retrieval.RetrievalService
}

// NewDefaultToolRegistry creates a new DefaultToolRegistry.
func NewDefaultToolRegistry(r *retrieval.RetrievalService) *DefaultToolRegistry {
	return &DefaultToolRegistry{
		retrieval: r,
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
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
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

	// Always save as "candidate" to avoid conflicts, letting the background worker consolidate.
	mem, err := r.retrieval.CreateMemory(ctx, userID, content, importance, "candidate", pqtype.NullRawMessage{})
	if err != nil {
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

	results, err := r.retrieval.SearchSimilar(ctx, userID, query, limit)
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
