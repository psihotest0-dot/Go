package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

// Orchestrator orchestrates the main agent pipeline.
type Orchestrator struct {
	db               *db.Database
	retrieval        *retrieval.RetrievalService
	llmClient        llm.LLMClient
	tools            ToolRegistry
	sessionSummaries sync.Map
}

// NewOrchestrator creates a new Orchestrator.
func NewOrchestrator(database *db.Database, r *retrieval.RetrievalService, l llm.LLMClient, t ToolRegistry) *Orchestrator {
	return &Orchestrator{
		db:        database,
		retrieval: r,
		llmClient: l,
		tools:     t,
	}
}

// HandleStreamChat handles incoming chat messages and streams the response.
func (o *Orchestrator) HandleStreamChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)

	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("invalid request body", slog.Any("error", err), slog.String("path", r.URL.Path))
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" || req.Content == "" {
		slog.Warn("missing session_id or content", slog.String("path", r.URL.Path))
		http.Error(w, "session_id and content are required", http.StatusBadRequest)
		return
	}

	if err := o.saveUserMessage(r.Context(), userID, req.SessionID, req.Content); err != nil {
		slog.Error("failed to save user message", "err", err)
		http.Error(w, fmt.Sprintf("failed to save user message: %v", err), http.StatusInternalServerError)
		return
	}

	chatCtx, memoryIDs, err := o.loadContext(r.Context(), userID, req)
	if err != nil {
		return
	}

	_ = o.writeTrace(r.Context(), userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history, oldMessages, droppedCount := pruneHistory(chatCtx.History)
	chatCtx.RollingSum = o.rollingSummary(r.Context(), req.SessionID, oldMessages, droppedCount)

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		slog.Error("failed to render system prompt", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)
	finalMessages = append(finalMessages, history...)

	_ = o.writeTrace(r.Context(), userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(finalMessages),
	})

	tools := toolDefinitions()

	assistantContent, toolCalls, err := o.streamResponse(w, r, finalMessages, tools, chatCtx.Sources)
	if err != nil {
		return
	}

	_ = o.persistAssistantMessage(context.Background(), userID, req.SessionID, assistantContent, toolCalls, chatCtx.Sources)

	_ = o.writeTrace(context.Background(), userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})
}

// ProcessChat executes the chat pipeline without HTTP dependencies.
func (o *Orchestrator) ProcessChat(ctx context.Context, userID uuid.UUID, req ChatRequest) (string, error) {
	if req.SessionID == "" || req.Content == "" {
		return "", fmt.Errorf("session_id and content are required")
	}

	if err := o.saveUserMessage(ctx, userID, req.SessionID, req.Content); err != nil {
		slog.Error("failed to save user message", "err", err)
		return "", fmt.Errorf("failed to save user message: %w", err)
	}

	chatCtx, memoryIDs, err := o.loadContext(ctx, userID, req)
	if err != nil {
		return "", err
	}

	_ = o.writeTrace(ctx, userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history, oldMessages, droppedCount := pruneHistory(chatCtx.History)
	chatCtx.RollingSum = o.rollingSummary(ctx, req.SessionID, oldMessages, droppedCount)

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		slog.Error("failed to render system prompt", "err", err)
		return "", fmt.Errorf("failed to render system prompt: %w", err)
	}

	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)
	finalMessages = append(finalMessages, history...)

	_ = o.writeTrace(ctx, userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(finalMessages),
	})

	tools := toolDefinitions()

	assistantContent, toolCalls, err := o.generateResponse(ctx, finalMessages, tools, chatCtx.Sources)
	if err != nil {
		return "", err
	}

	// Use background context for persistence so it is not interrupted by immediate request context cancellation.
	_ = o.persistAssistantMessage(context.Background(), userID, req.SessionID, assistantContent, toolCalls, chatCtx.Sources)

	_ = o.writeTrace(context.Background(), userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})

	return assistantContent, nil
}
