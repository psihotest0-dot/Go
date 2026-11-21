package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/attachments"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/proposals"
	"go-monolith/internal/retrieval"
)

// persistTimeout bounds the writes after an answer (storing it, the trace). Each
// such context is started when the answer is complete: one made before the
// generation would expire during a long answer and the answer would be lost.
// It is a variable so a test can shorten it.
var persistTimeout = 30 * time.Second

// chatRequestTimeout bounds one chat request (a new turn or a regeneration):
// preparing the prompt, every tool round and the streamed answer. A cancelled
// client ends it sooner; storing the answer has its own persistTimeout.
const chatRequestTimeout = 5 * time.Minute

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
	// windows remembers where each session's history window began, so that it
	// is cut in steps and not slid on every turn (see historyWindows).
	windows historyWindows
	// sessions gives each conversation one turn at a time (see sessionLocks).
	sessions sessionLocks
	// budgets are the token limits requests are built with; the zero value
	// means the built-in ones (see limits).
	budgets budgets
	// adaptiveSkip is ENABLE_ADAPTIVE_SKIP, read once when the orchestrator is built
	// (it used to be read from the environment on every chat request).
	adaptiveSkip bool
	// attachments holds the files attached to a conversation; nil means none can be.
	attachments *attachments.Service
	// groups are the tools that are not on offer until the model asks for them (see
	// toolGroup).
	groups []toolGroup
	// addToLibrary: the tool add_to_library is offered in a turn whose conversation has a
	// link the person wrote (see offersLibraryAdd).
	addToLibrary bool
	// paid is the paid model of the chat (nil: none), paidName its name; libraryPaid says that
	// the questions to the library go to it on their own. See model_routing.go.
	paid        llm.LLMClient
	paidName    string
	libraryPaid bool
	// questionEvery is CHAT_QUESTION_EVERY: every this-many-th reply of a conversation ends
	// with a question; 0, the value of an Orchestrator built without NewOrchestrator, puts
	// the dose off. See closing_note.go.
	questionEvery int
}

// WithFileTools offers the tools for the user's own files (plan item P44) as a group:
// their stub is on offer in every turn, the tools themselves after the model called it.
// The registry must have the folder (DefaultToolRegistry.WithFiles).
func (o *Orchestrator) WithFileTools() *Orchestrator {
	o.groups = append(o.groups, filesGroup())
	return o
}

// WithLibraryAdd offers the tool add_to_library (plan item P53) in a conversation where the
// person wrote a link. The registry must have its fetcher and adder
// (DefaultToolRegistry.WithLibraryAdd).
func (o *Orchestrator) WithLibraryAdd() *Orchestrator {
	o.addToLibrary = true
	return o
}

// WithAttachments lets the chat show the files attached to a conversation.
func (o *Orchestrator) WithAttachments(s *attachments.Service) *Orchestrator {
	o.attachments = s
	return o
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

		questionEvery: cfg.ChatQuestionEvery,
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

	ctx, cancel := context.WithTimeout(r.Context(), chatRequestTimeout)
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
	r = r.WithContext(WithLLMChoice(r.Context(), req.UsePaid))

	// One turn at a time in a conversation, from the question to the stored answer: the
	// next one (the app and Telegram share a conversation) reads the history only after this
	// answer is in it. The wait is part of the request's time.
	streaming := false
	unlock, err := o.sessions.lock(ctx, userID, req.SessionID, func() {
		if req.QueueEvents {
			streaming = beginQueuedStream(w)
		}
	})
	if err != nil {
		o.refuseBusy(ctx, w, streaming, err, req.SessionID)
		return
	}
	defer unlock()

	// The user turn is stored before generation because the prompt history is
	// read back from the database. A failed generation therefore leaves the
	// question without an answer; /chat/regenerate is the recovery path, so the
	// turn is deliberately kept instead of being rolled back on every transient
	// LLM error.
	if err := o.saveUserMessage(r.Context(), userID, req.SessionID, req.Content); err != nil {
		slog.ErrorContext(ctx, "failed to save user message", "err", err)
		chatFailure(w, streaming, http.StatusInternalServerError, "internal server error")
		return
	}

	prep, err := o.prepareTurn(r.Context(), userID, req, false)
	if err != nil {
		// Streaming has not started (unless the client waited in a queue and was told so),
		// so the client can still receive a normal HTTP error instead of a "200 OK" with an
		// empty body that leaves the caller waiting for events that never arrive.
		slog.ErrorContext(ctx, "failed to prepare the chat turn", "err", err, "session_id", req.SessionID)
		chatFailure(w, streaming, http.StatusInternalServerError, "internal server error")
		return
	}
	r = r.WithContext(prep.ctx)

	// The answer is stored before the final [DONE] frame (see streamResponse), on
	// its own context: a client that leaves right after the last piece of text
	// must not abort the write. The context starts when the answer is complete,
	// not before the generation, which can take longer than its timeout.
	store := func(content string, toolCalls []openai.ToolCall) {
		storeCtx, cancelStore := context.WithTimeout(context.Background(), persistTimeout)
		defer cancelStore()
		o.storeAnswer(storeCtx, userID, req.SessionID, content, toolCalls, answerSources(prep.turn, prep.sources))
	}

	assistantContent, _, err := o.streamResponse(w, r, req.SessionID, prep.messages, prep.tools, prep.sources, store)
	if err != nil {
		// The response is already committed and the reason has been sent to the
		// client as an SSE error frame, so only log it here; a partial answer is
		// never stored.
		slog.ErrorContext(ctx, "stream failed", "err", err, "session_id", req.SessionID)
		return
	}

	bgCtx, cancelBg := context.WithTimeout(context.Background(), persistTimeout)
	defer cancelBg()
	o.trace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})
}

type RegenerateRequest struct {
	SessionID string `json:"session_id"`
	// UsePaid is the same choice as in ChatRequest.
	UsePaid *bool `json:"use_paid,omitempty"`
	// QueueEvents is the same as in ChatRequest.
	QueueEvents bool `json:"queue_events,omitempty"`
}

// HandleRegenerate handles regenerating the last assistant response.
func (o *Orchestrator) HandleRegenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.WarnContext(r.Context(), "method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), chatRequestTimeout)
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

	// Like a new turn, a regeneration has the conversation to itself: the answer it replaces
	// is picked out below, and a turn that came in meanwhile must not lose its answer to it.
	streaming := false
	unlock, err := o.sessions.lock(ctx, userID, regenReq.SessionID, func() {
		if regenReq.QueueEvents {
			streaming = beginQueuedStream(w)
		}
	})
	if err != nil {
		o.refuseBusy(ctx, w, streaming, err, regenReq.SessionID)
		return
	}
	defer unlock()

	dbMsgs, err := o.db.Queries.GetMessagesBySession(r.Context(), db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: regenReq.SessionID,
		Limit:     int32(o.limits().historyMessages),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to get messages", "err", err)
		chatFailure(w, streaming, http.StatusInternalServerError, "internal server error")
		return
	}

	// The newest messages come first. The answers after the last question are the ones
	// the regeneration replaces (and leaves out of the prompt, see prepareTurn); none when
	// the turn had failed. They are taken by id now, so that a turn sent meanwhile does
	// not lose its answer at the end.
	var lastUserContent string
	var replaced []uuid.UUID
	for _, m := range dbMsgs {
		if m.Role == "user" {
			lastUserContent = m.Content
			break
		}
		if m.Role == "assistant" {
			replaced = append(replaced, m.ID)
		}
	}

	if lastUserContent == "" {
		slog.WarnContext(ctx, "no user message found to regenerate from", slog.String("session_id", regenReq.SessionID))
		chatFailure(w, streaming, http.StatusBadRequest, "no user message found to regenerate from")
		return
	}

	req := ChatRequest{
		SessionID: regenReq.SessionID,
		Content:   lastUserContent,
		UsePaid:   regenReq.UsePaid,
	}

	// See HandleStreamChat: tool calls fall back to the user turn when the
	// model omits the "query" argument.
	r = r.WithContext(WithCurrentQuery(r.Context(), req.Content))
	r = r.WithContext(WithLLMChoice(r.Context(), req.UsePaid))

	prep, err := o.prepareTurn(r.Context(), userID, req, true)
	if err != nil {
		// Streaming has not started (unless the client waited in a queue and was told so), so
		// the client can still receive a normal HTTP error instead of a "200 OK" with an
		// empty body.
		slog.ErrorContext(ctx, "failed to prepare the chat turn", "err", err, "session_id", req.SessionID)
		chatFailure(w, streaming, http.StatusInternalServerError, "internal server error")
		return
	}
	r = r.WithContext(prep.ctx)

	// The answer is complete when replace runs (just before [DONE]), so the write
	// phase gets its own context, started then, and is not aborted by a client
	// that disconnects right after the last piece of text.
	replace := func(content string, toolCalls []openai.ToolCall) {
		storeCtx, cancelStore := context.WithTimeout(context.Background(), persistTimeout)
		defer cancelStore()
		// A model that only "thought" leaves nothing to show; keep the old answer
		// rather than replacing it with an empty one.
		if sanitizeAssistantText(content) == "" && len(toolCalls) == 0 {
			slog.WarnContext(ctx, "regenerated answer is empty; keeping the previous one", "session_id", req.SessionID)
			return
		}
		// The old answer goes only now that a complete new one exists, and in the
		// same transaction as the new one is written.
		if err := o.replaceAnswer(storeCtx, userID, req.SessionID, replaced, content, toolCalls, answerSources(prep.turn, prep.sources)); err != nil {
			slog.ErrorContext(ctx, "failed to replace the previous answer; it is kept", "err", err, "session_id", req.SessionID)
		}
	}

	assistantContent, _, err := o.streamResponse(w, r, req.SessionID, prep.messages, prep.tools, prep.sources, replace)
	if err != nil {
		// The previous answer is untouched, so a failed regeneration leaves the
		// session as it was instead of losing the reply it replaced.
		slog.ErrorContext(ctx, "stream failed", "err", err, "session_id", req.SessionID)
		return
	}

	bgCtx, cancelBg := context.WithTimeout(context.Background(), persistTimeout)
	defer cancelBg()
	o.trace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(assistantContent),
	})
}

// refuseBusy answers a request that waited for its conversation and ran out of time. A client
// that has left (cancelled) is owed nothing.
func (o *Orchestrator) refuseBusy(ctx context.Context, w http.ResponseWriter, streaming bool, err error, sessionID string) {
	if errors.Is(err, context.Canceled) {
		slog.InfoContext(ctx, "the client left while it waited for its conversation", "session_id", sessionID)
		return
	}
	slog.WarnContext(ctx, "the conversation stayed busy", "err", err, "session_id", sessionID)
	chatFailure(w, streaming, http.StatusServiceUnavailable, busyMessage)
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

	// See HandleStreamChat: one turn at a time in a conversation. The caller's time
	// (Telegram: chatTurnTimeout) covers the wait; when it ends first nothing was stored.
	unlock, err := o.sessions.lock(ctx, userID, req.SessionID, nil)
	if err != nil {
		slog.WarnContext(ctx, "the conversation stayed busy", "err", err, "session_id", req.SessionID)
		return ChatTurn{}, fmt.Errorf("%w: %v", ErrConversationBusy, err)
	}
	defer unlock()

	// See HandleStreamChat: the user turn is persisted up front so that it is
	// part of the history the prompt is built from.
	if err := o.saveUserMessage(ctx, userID, req.SessionID, req.Content); err != nil {
		slog.ErrorContext(ctx, "failed to save user message", "err", err)
		return ChatTurn{}, fmt.Errorf("failed to save user message: %w", err)
	}

	prep, err := o.prepareTurn(ctx, userID, req, false)
	if err != nil {
		return ChatTurn{}, err
	}
	ctx = prep.ctx

	assistantContent, _, toolCalls, err := o.generateResponse(ctx, prep.messages, prep.tools, nil)
	if err != nil {
		return ChatTurn{}, err
	}
	answer := sanitizeAssistantText(assistantContent)

	// Use background context for persistence so it is not interrupted by immediate request context cancellation.
	bgCtx, cancelBg := context.WithTimeout(context.Background(), persistTimeout)
	defer cancelBg()
	o.storeAnswer(bgCtx, userID, req.SessionID, answer, toolCalls, answerSources(prep.turn, prep.sources))

	o.trace(bgCtx, userID, req.SessionID, "stream_end", map[string]interface{}{
		"response_length": len(answer),
	})

	return ChatTurn{Answer: answer, Proposals: prep.turn.madeProposals()}, nil
}
