package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
	db        *db.Database
	retrieval *retrieval.RetrievalService
	llmClient llm.LLMClient
	tools     ToolRegistry
	// summaries caches rolling summaries per session. The cache is bounded so
	// that client supplied session IDs cannot grow memory without limit.
	summaries *sessionSummaryStore
}

// NewOrchestrator creates a new Orchestrator.
func NewOrchestrator(database *db.Database, r *retrieval.RetrievalService, l llm.LLMClient, t ToolRegistry) *Orchestrator {
	return &Orchestrator{
		db:        database,
		retrieval: r,
		llmClient: l,
		tools:     t,
		summaries: newSessionSummaryStore(),
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
	if !readJSONBody(w, r, &req) {
		return
	}

	if req.SessionID == "" || req.Content == "" {
		slog.Warn("missing session_id or content", slog.String("path", r.URL.Path))
		http.Error(w, "session_id and content are required", http.StatusBadRequest)
		return
	}

	if err := validateUserMessage(req.Content); err != nil {
		slog.Warn("message too long",
			slog.Any("error", err),
			slog.String("session_id", req.SessionID))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The user turn is stored before generation because the prompt history is
	// read back from the database. A failed generation therefore leaves the
	// question without an answer; /chat/regenerate is the recovery path, so the
	// turn is deliberately kept instead of being rolled back on every transient
	// LLM error.
	if err := o.saveUserMessage(r.Context(), userID, req.SessionID, req.Content); err != nil {
		slog.Error("failed to save user message", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	chatCtx, memoryIDs, err := o.loadContext(r.Context(), userID, req)
	if err != nil {
		// Streaming has not started, so the client can still receive a normal
		// HTTP error instead of a "200 OK" with an empty body that leaves the
		// caller waiting for events that never arrive.
		slog.Error("failed to load context", "err", err, "session_id", req.SessionID)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	_ = o.writeTrace(r.Context(), userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history, oldMessages, droppedCount := pruneHistory(chatCtx.History)
	chatCtx.RollingSum = o.rollingSummary(userID, req.SessionID, oldMessages, droppedCount)

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

	assistantContent, toolCalls, err := o.streamResponse(w, r, req.SessionID, finalMessages, tools, chatCtx.Sources)
	if err != nil {
		// The response is already committed and the reason has been sent to the
		// client as an SSE error frame, so only log it here and skip persisting
		// a partial answer.
		slog.Error("stream failed", "err", err, "session_id", req.SessionID)
		return
	}

	bgCtx, cancelBg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBg()
	_ = o.persistAssistantMessage(bgCtx, userID, req.SessionID, assistantContent, toolCalls, chatCtx.Sources)

	_ = o.writeTrace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})
}

type RegenerateRequest struct {
	SessionID string `json:"session_id"`
}

// HandleRegenerate handles regenerating the last assistant response.
func (o *Orchestrator) HandleRegenerate(w http.ResponseWriter, r *http.Request) {
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

	var regenReq RegenerateRequest
	if !readJSONBody(w, r, &regenReq) {
		return
	}

	if regenReq.SessionID == "" {
		slog.Warn("missing session_id", slog.String("path", r.URL.Path))
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	dbMsgs, err := o.db.Queries.GetMessagesBySession(r.Context(), db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: regenReq.SessionID,
		Limit:     maxHistoryMessages,
	})
	if err != nil {
		slog.Error("failed to get messages", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var lastUserContent string
	for _, m := range dbMsgs {
		if m.Role == "user" {
			lastUserContent = m.Content
			break
		}
	}

	if lastUserContent == "" {
		slog.Warn("no user message found to regenerate from", slog.String("session_id", regenReq.SessionID))
		http.Error(w, "no user message found to regenerate from", http.StatusBadRequest)
		return
	}

	req := ChatRequest{
		SessionID: regenReq.SessionID,
		Content:   lastUserContent,
	}

	chatCtx, memoryIDs, err := o.loadContext(r.Context(), userID, req)
	if err != nil {
		// Streaming has not started, so the client can still receive a normal
		// HTTP error instead of a "200 OK" with an empty body.
		slog.Error("failed to load context", "err", err, "session_id", req.SessionID)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// The previous answer is still persisted, so it is fetched as part of the
	// history. Drop it from the prompt: the model must regenerate the reply,
	// not read (and copy) the one it is replacing.
	chatCtx.History = dropTrailingAssistantMessages(chatCtx.History)

	_ = o.writeTrace(r.Context(), userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history, oldMessages, droppedCount := pruneHistory(chatCtx.History)
	chatCtx.RollingSum = o.rollingSummary(userID, req.SessionID, oldMessages, droppedCount)

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

	assistantContent, toolCalls, err := o.streamResponse(w, r, req.SessionID, finalMessages, tools, chatCtx.Sources)
	if err != nil {
		// The previous answer is untouched, so a failed regeneration leaves the
		// session as it was instead of losing the reply it replaced.
		slog.Error("stream failed", "err", err, "session_id", req.SessionID)
		return
	}

	// The answer is already complete, so the write phase must not be aborted by
	// a client that disconnects right after the final event.
	bgCtx, cancelBg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBg()

	// Replace the old answer only now that a complete new one exists.
	if err := o.db.Queries.DeleteLastAssistantMessage(bgCtx, db.DeleteLastAssistantMessageParams{
		SessionID: req.SessionID,
		UserID:    userID,
	}); err != nil {
		// Persisting as well would leave two answers for the same question, so
		// keep the stored history consistent and drop the new one.
		slog.Error("failed to delete last assistant message", "err", err, "session_id", req.SessionID)
		return
	}

	_ = o.persistAssistantMessage(bgCtx, userID, req.SessionID, assistantContent, toolCalls, chatCtx.Sources)

	_ = o.writeTrace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})
}

// ProcessChat executes the chat pipeline without HTTP dependencies.
func (o *Orchestrator) ProcessChat(ctx context.Context, userID uuid.UUID, req ChatRequest) (string, string, error) {
	ctx = context.WithValue(ctx, api.ParsedUserIDKey, userID)

	if req.SessionID == "" || req.Content == "" {
		return "", "", fmt.Errorf("session_id and content are required")
	}

	// See HandleStreamChat: the user turn is persisted up front so that it is
	// part of the history the prompt is built from.
	if err := o.saveUserMessage(ctx, userID, req.SessionID, req.Content); err != nil {
		slog.Error("failed to save user message", "err", err)
		return "", "", fmt.Errorf("failed to save user message: %w", err)
	}

	chatCtx, memoryIDs, err := o.loadContext(ctx, userID, req)
	if err != nil {
		return "", "", err
	}

	_ = o.writeTrace(ctx, userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history, oldMessages, droppedCount := pruneHistory(chatCtx.History)
	chatCtx.RollingSum = o.rollingSummary(userID, req.SessionID, oldMessages, droppedCount)

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		slog.Error("failed to render system prompt", "err", err)
		return "", "", fmt.Errorf("failed to render system prompt: %w", err)
	}

	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)
	finalMessages = append(finalMessages, history...)

	_ = o.writeTrace(ctx, userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(finalMessages),
	})

	tools := toolDefinitions()

	assistantContent, reasoningContent, toolCalls, err := o.generateResponse(ctx, finalMessages, tools, nil)
	if err != nil {
		return "", "", err
	}

	// Use background context for persistence so it is not interrupted by immediate request context cancellation.
	bgCtx, cancelBg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBg()
	_ = o.persistAssistantMessage(bgCtx, userID, req.SessionID, assistantContent, toolCalls, chatCtx.Sources)

	_ = o.writeTrace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})

	return assistantContent, reasoningContent, nil
}
