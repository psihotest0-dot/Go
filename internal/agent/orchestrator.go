package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/proposals"
	"go-monolith/internal/retrieval"
)

// Orchestrator orchestrates the main agent pipeline.
type Orchestrator struct {
	db        *db.Database
	retrieval *retrieval.RetrievalService
	llmClient llm.LLMClient
	tools     ToolRegistry
	// summaryRuns and summaryWG track the background passes that update the
	// rolling summary: one per session at a time, and a wait group so tests (and
	// a future graceful shutdown) can wait for them.
	summaryRuns summaryRuns
	summaryWG   sync.WaitGroup
	// budgets are the token limits requests are built with; the zero value
	// means the built-in ones (see limits).
	budgets budgets
	// adaptiveSkip is ENABLE_ADAPTIVE_SKIP, read once when the orchestrator is built
	// (it used to be read from the environment on every chat request).
	adaptiveSkip bool
}

// NewOrchestrator creates a new Orchestrator.
func NewOrchestrator(database *db.Database, r *retrieval.RetrievalService, l llm.LLMClient, t ToolRegistry) *Orchestrator {
	cfg := config.Load()
	b := newBudgets(cfg)
	b.log()

	return &Orchestrator{
		db:           database,
		retrieval:    r,
		llmClient:    l,
		tools:        t,
		budgets:      b,
		adaptiveSkip: cfg.EnableAdaptiveSkip,
	}
}

// limits returns the token budgets in effect. An Orchestrator built without
// NewOrchestrator (tests) has none configured and uses the built-in ones.
func (o *Orchestrator) limits() budgets {
	if o.budgets == (budgets{}) {
		return legacyBudgets()
	}
	return o.budgets
}

// HandleStreamChat handles incoming chat messages and streams the response.
func (o *Orchestrator) HandleStreamChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.WarnContext(r.Context(), "method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)

	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.WarnContext(ctx, "unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req ChatRequest
	if !readJSONBody(w, r, &req) {
		return
	}

	if req.SessionID == "" || req.Content == "" {
		slog.WarnContext(ctx, "missing session_id or content", slog.String("path", r.URL.Path))
		http.Error(w, "session_id and content are required", http.StatusBadRequest)
		return
	}

	if err := validateUserMessage(req.Content); err != nil {
		slog.WarnContext(ctx, "message too long",
			slog.Any("error", err),
			slog.String("session_id", req.SessionID))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Tool calls carry only the arguments the model chose to send. Remember the
	// question so the search tools can fall back to it when the model omits
	// "query" (small local models regularly send an empty argument object).
	r = r.WithContext(WithCurrentQuery(r.Context(), req.Content))

	// The user turn is stored before generation because the prompt history is
	// read back from the database. A failed generation therefore leaves the
	// question without an answer; /chat/regenerate is the recovery path, so the
	// turn is deliberately kept instead of being rolled back on every transient
	// LLM error.
	if err := o.saveUserMessage(r.Context(), userID, req.SessionID, req.Content); err != nil {
		slog.ErrorContext(ctx, "failed to save user message", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	chatCtx, memoryIDs, err := o.loadContext(r.Context(), userID, req)
	if err != nil {
		// Streaming has not started, so the client can still receive a normal
		// HTTP error instead of a "200 OK" with an empty body that leaves the
		// caller waiting for events that never arrive.
		slog.ErrorContext(ctx, "failed to load context", "err", err, "session_id", req.SessionID)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// The tools of this turn share its state: what untrusted text it has read and
	// which proposals it made (see turnState).
	turnCtx, _ := withTurn(r.Context(), req.SessionID, len(chatCtx.Chunks) > 0)
	r = r.WithContext(turnCtx)

	o.trace(r.Context(), userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history := pruneHistory(chatCtx.History, o.limits().historyTokens, o.limits().historyMessages)
	chatCtx.RollingSum = o.rollingSummary(r.Context(), userID, req.SessionID, chatCtx.windowStart(len(history)))

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to render system prompt", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)
	finalMessages = append(finalMessages, history...)

	o.trace(r.Context(), userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(finalMessages),
	})

	tools := toolDefinitions()

	assistantContent, toolCalls, err := o.streamResponse(w, r, req.SessionID, finalMessages, tools, chatCtx.Sources)
	if err != nil {
		// The response is already committed and the reason has been sent to the
		// client as an SSE error frame, so only log it here and skip persisting
		// a partial answer.
		slog.ErrorContext(ctx, "stream failed", "err", err, "session_id", req.SessionID)
		return
	}

	bgCtx, cancelBg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBg()
	o.storeAnswer(bgCtx, userID, req.SessionID, assistantContent, toolCalls, chatCtx.Sources)

	o.trace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})
}

type RegenerateRequest struct {
	SessionID string `json:"session_id"`
}

// HandleRegenerate handles regenerating the last assistant response.
func (o *Orchestrator) HandleRegenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.WarnContext(r.Context(), "method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)

	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.WarnContext(ctx, "unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var regenReq RegenerateRequest
	if !readJSONBody(w, r, &regenReq) {
		return
	}

	if regenReq.SessionID == "" {
		slog.WarnContext(ctx, "missing session_id", slog.String("path", r.URL.Path))
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	dbMsgs, err := o.db.Queries.GetMessagesBySession(r.Context(), db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: regenReq.SessionID,
		Limit:     int32(o.limits().historyMessages),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to get messages", "err", err)
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
		slog.WarnContext(ctx, "no user message found to regenerate from", slog.String("session_id", regenReq.SessionID))
		http.Error(w, "no user message found to regenerate from", http.StatusBadRequest)
		return
	}

	req := ChatRequest{
		SessionID: regenReq.SessionID,
		Content:   lastUserContent,
	}

	// See HandleStreamChat: tool calls fall back to the user turn when the
	// model omits the "query" argument.
	r = r.WithContext(WithCurrentQuery(r.Context(), req.Content))

	chatCtx, memoryIDs, err := o.loadContext(r.Context(), userID, req)
	if err != nil {
		// Streaming has not started, so the client can still receive a normal
		// HTTP error instead of a "200 OK" with an empty body.
		slog.ErrorContext(ctx, "failed to load context", "err", err, "session_id", req.SessionID)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// The previous answer is still persisted, so it is fetched as part of the
	// history. Drop it from the prompt: the model must regenerate the reply,
	// not read (and copy) the one it is replacing.
	chatCtx.History = dropTrailingAssistantMessages(chatCtx.History)
	if len(chatCtx.HistoryAt) > len(chatCtx.History) {
		chatCtx.HistoryAt = chatCtx.HistoryAt[:len(chatCtx.History)]
	}

	// The tools of this turn share its state: what untrusted text it has read and
	// which proposals it made (see turnState).
	turnCtx, _ := withTurn(r.Context(), req.SessionID, len(chatCtx.Chunks) > 0)
	r = r.WithContext(turnCtx)

	o.trace(r.Context(), userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history := pruneHistory(chatCtx.History, o.limits().historyTokens, o.limits().historyMessages)
	chatCtx.RollingSum = o.rollingSummary(r.Context(), userID, req.SessionID, chatCtx.windowStart(len(history)))

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to render system prompt", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)
	finalMessages = append(finalMessages, history...)

	o.trace(r.Context(), userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(finalMessages),
	})

	tools := toolDefinitions()

	assistantContent, toolCalls, err := o.streamResponse(w, r, req.SessionID, finalMessages, tools, chatCtx.Sources)
	if err != nil {
		// The previous answer is untouched, so a failed regeneration leaves the
		// session as it was instead of losing the reply it replaced.
		slog.ErrorContext(ctx, "stream failed", "err", err, "session_id", req.SessionID)
		return
	}

	// The answer is already complete, so the write phase must not be aborted by
	// a client that disconnects right after the final event.
	bgCtx, cancelBg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBg()

	// A model that only "thought" leaves nothing to show; keep the old answer
	// rather than replacing it with an empty one.
	if sanitizeAssistantText(assistantContent) == "" && len(toolCalls) == 0 {
		slog.WarnContext(ctx, "regenerated answer is empty; keeping the previous one", "session_id", req.SessionID)
		return
	}

	// Replace the old answer only now that a complete new one exists.
	if err := o.db.Queries.DeleteLastAssistantMessage(bgCtx, db.DeleteLastAssistantMessageParams{
		SessionID: req.SessionID,
		UserID:    userID,
	}); err != nil {
		// Persisting as well would leave two answers for the same question, so
		// keep the stored history consistent and drop the new one.
		slog.ErrorContext(ctx, "failed to delete last assistant message", "err", err, "session_id", req.SessionID)
		return
	}

	o.storeAnswer(bgCtx, userID, req.SessionID, assistantContent, toolCalls, chatCtx.Sources)

	o.trace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})
}

// ChatTurn is the outcome of a chat turn run without HTTP.
type ChatTurn struct {
	// Answer has the hidden reasoning and the tool markup already removed.
	Answer string
	// Proposals are the writes the model asked for in this turn that wait for the
	// user's confirmation.
	Proposals []proposals.Proposal
}

// ProcessChat executes the chat pipeline without HTTP dependencies and returns
// the answer with hidden reasoning and tool markup already removed.
func (o *Orchestrator) ProcessChat(ctx context.Context, userID uuid.UUID, req ChatRequest) (string, error) {
	turn, err := o.ProcessChatTurn(ctx, userID, req)
	return turn.Answer, err
}

// ProcessChatTurn is ProcessChat that also reports the proposals of the turn, so
// a caller without an event stream (Telegram) can ask the user about them.
func (o *Orchestrator) ProcessChatTurn(ctx context.Context, userID uuid.UUID, req ChatRequest) (ChatTurn, error) {
	ctx = context.WithValue(ctx, api.ParsedUserIDKey, userID)
	ctx = WithCurrentQuery(ctx, req.Content)

	if req.SessionID == "" || req.Content == "" {
		return ChatTurn{}, fmt.Errorf("session_id and content are required")
	}

	// See HandleStreamChat: the user turn is persisted up front so that it is
	// part of the history the prompt is built from.
	if err := o.saveUserMessage(ctx, userID, req.SessionID, req.Content); err != nil {
		slog.ErrorContext(ctx, "failed to save user message", "err", err)
		return ChatTurn{}, fmt.Errorf("failed to save user message: %w", err)
	}

	chatCtx, memoryIDs, err := o.loadContext(ctx, userID, req)
	if err != nil {
		return ChatTurn{}, err
	}

	ctx, turn := withTurn(ctx, req.SessionID, len(chatCtx.Chunks) > 0)

	o.trace(ctx, userID, req.SessionID, "search", map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})

	history := pruneHistory(chatCtx.History, o.limits().historyTokens, o.limits().historyMessages)
	chatCtx.RollingSum = o.rollingSummary(ctx, userID, req.SessionID, chatCtx.windowStart(len(history)))

	systemMessage, err := buildSystemPrompt(chatCtx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to render system prompt", "err", err)
		return ChatTurn{}, fmt.Errorf("failed to render system prompt: %w", err)
	}

	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)
	finalMessages = append(finalMessages, history...)

	o.trace(ctx, userID, req.SessionID, "prune", map[string]interface{}{
		"total_messages": len(chatCtx.History) + 1,
		"pruned_to":      len(finalMessages),
	})

	tools := toolDefinitions()

	assistantContent, _, toolCalls, err := o.generateResponse(ctx, finalMessages, tools, nil)
	if err != nil {
		return ChatTurn{}, err
	}
	answer := sanitizeAssistantText(assistantContent)

	// Use background context for persistence so it is not interrupted by immediate request context cancellation.
	bgCtx, cancelBg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBg()
	o.storeAnswer(bgCtx, userID, req.SessionID, answer, toolCalls, chatCtx.Sources)

	o.trace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(answer),
	})

	return ChatTurn{Answer: answer, Proposals: turn.madeProposals()}, nil
}
