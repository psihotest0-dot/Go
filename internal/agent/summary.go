package agent

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
)

// The rolling summary is a digest of the messages that no longer fit into the
// prompt. It is stored in the database (session_summaries) together with its
// position: every message of the session created at or before covered_until_at
// is part of it. A message is therefore in exactly one place - the summary, the
// prompt, or (briefly) the gap between them until the next pass picks it up.
const (
	// summaryMinNewMessages is how many messages must wait outside the prompt
	// and the summary before a new pass is started.
	summaryMinNewMessages = 6
	// summaryTimeout bounds a single summarisation LLM call, including the wait
	// for a free background slot.
	summaryTimeout = 2 * time.Minute
	// summarySaveTimeout bounds the database write that follows it.
	summarySaveTimeout = 10 * time.Second
	// maxSummaryTokens caps the retained summary so a runaway model response
	// cannot grow the system prompt past the context window.
	maxSummaryTokens = 1000
	// maxSummaryAnswerTokens is the answer limit of the summarising request.
	maxSummaryAnswerTokens = 600
	// maxSummaryInputMessages, maxSummaryInputTokens and
	// maxSummaryMessageTokens bound what one pass reads. A longer backlog is
	// worked off over several passes, oldest messages first.
	maxSummaryInputMessages = 40
	maxSummaryInputTokens   = 3000
	maxSummaryMessageTokens = 500
)

// summaryKey scopes a summary to its owner. Session IDs are client supplied, so
// keying by session alone would let two users who pick the same session ID
// block each other's summarising.
func summaryKey(userID uuid.UUID, sessionID string) string {
	return userID.String() + "\x00" + sessionID
}

// summaryRuns tracks the sessions that have a summarising pass in flight, so a
// second turn of the same session does not start a duplicate pass. The zero
// value is ready to use.
type summaryRuns struct {
	mu      sync.Mutex
	running map[string]struct{}
}

// begin marks key as running and reports whether it was free.
func (s *summaryRuns) begin(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, busy := s.running[key]; busy {
		return false
	}
	if s.running == nil {
		s.running = make(map[string]struct{})
	}
	s.running[key] = struct{}{}
	return true
}

func (s *summaryRuns) end(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, key)
}

// rollingSummary returns the stored summary of the session and, when enough
// messages are waiting outside both the summary and the prompt, starts a pass in
// the background that folds them in. promptStart is the time of the oldest
// message that goes into the prompt; everything before it is the summary's
// business, regardless of how many messages the prompt reads.
func (o *Orchestrator) rollingSummary(ctx context.Context, userID uuid.UUID, sessionID string, promptStart time.Time) string {
	if o.db == nil {
		return ""
	}

	var summary string
	var covered time.Time // the zero time covers nothing

	stored, err := o.db.Queries.GetSessionSummary(ctx, db.GetSessionSummaryParams{UserID: userID, SessionID: sessionID})
	switch {
	case err == nil:
		summary, covered = stored.Summary, stored.CoveredUntilAt
	case !errors.Is(err, sql.ErrNoRows):
		// The summary is an enhancement: the chat goes on without it.
		slog.Error("failed to read the rolling summary", "err", err, "session_id", sessionID)
		return ""
	}

	if promptStart.IsZero() {
		return summary
	}

	pending, err := o.db.Queries.GetMessagesToSummarize(ctx, db.GetMessagesToSummarizeParams{
		UserID:    userID,
		SessionID: sessionID,
		After:     covered,
		Before:    promptStart,
		Limit:     maxSummaryInputMessages,
	})
	if err != nil {
		slog.Error("failed to find messages to summarise", "err", err, "session_id", sessionID)
		return summary
	}
	if len(pending) < summaryMinNewMessages {
		return summary
	}

	key := summaryKey(userID, sessionID)
	if !o.summaryRuns.begin(key) {
		// A pass for this session is already running.
		return summary
	}

	o.summaryWG.Add(1)
	go o.generateSummary(key, userID, sessionID, summary, pending)

	return summary
}

// generateSummary folds the pending messages into the previous summary and
// stores the result. It runs in a detached goroutine, so it must never panic and
// must always clear the in-flight marker.
func (o *Orchestrator) generateSummary(key string, userID uuid.UUID, sessionID, previous string, pending []db.Message) {
	defer o.summaryWG.Done()
	defer o.summaryRuns.end(key)
	defer func() {
		if rcv := recover(); rcv != nil {
			slog.Error("panic while generating rolling summary", "panic", rcv, "session_id", sessionID, "user_id", userID)
		}
	}()

	if len(pending) == 0 {
		return
	}
	if prompts.GlobalManager == nil {
		slog.Error("prompt manager not initialised, skipping rolling summary", "session_id", sessionID)
		return
	}

	lines := make([]openai.ChatCompletionMessage, len(pending))
	for i, m := range pending {
		lines[i] = openai.ChatCompletionMessage{Role: m.Role, Content: m.Content}
	}
	text, included := summaryInputText(lines)
	if included == 0 {
		return
	}

	summaryInstructions, err := prompts.GlobalManager.Render("rolling_summary.tmpl", nil)
	if err != nil {
		slog.Error("failed to render rolling summary prompt", "err", err)
		return
	}
	summaryInput, err := prompts.GlobalManager.Render("rolling_summary_input.tmpl", map[string]interface{}{
		"PreviousSummary": previous,
		"MessagesText":    text,
	})
	if err != nil {
		slog.Error("failed to render rolling summary input", "err", err)
		return
	}

	// Background: the summary must not take a model slot from a chat turn.
	sumCtx, sumCancel := context.WithTimeout(llm.WithBackground(context.Background()), summaryTimeout)
	defer sumCancel()

	resp, err := o.llmClient.Chat(sumCtx, openai.ChatCompletionRequest{
		// The instructions are a system message and the conversation to digest is
		// the user message (a chat template needs a user turn to answer).
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: summaryInstructions},
			{Role: openai.ChatMessageRoleUser, Content: summaryInput},
		},
		MaxTokens: maxSummaryAnswerTokens,
		// The digest is a plain rewrite. Thinking would spend the whole answer
		// limit on reasoning and leave the summary empty.
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
		Temperature:        0.3,
	})
	if err != nil {
		slog.Error("failed to generate rolling summary", "err", err, "session_id", sessionID)
		return
	}
	if len(resp.Choices) == 0 {
		slog.Error("rolling summary response contained no choices", "session_id", sessionID)
		return
	}

	summary := truncateTokens(sanitizeAssistantText(resp.Choices[0].Message.Content), maxSummaryTokens)
	if summary == "" {
		// Marking the messages as covered by an empty summary would lose them.
		slog.Error("rolling summary came back empty; the messages stay pending", "session_id", sessionID)
		return
	}

	last := pending[included-1]
	saveCtx, saveCancel := context.WithTimeout(context.Background(), summarySaveTimeout)
	defer saveCancel()
	if err := o.db.Queries.UpsertSessionSummary(saveCtx, db.UpsertSessionSummaryParams{
		UserID:                userID,
		SessionID:             sessionID,
		Summary:               summary,
		CoveredUntilMessageID: last.ID,
		CoveredUntilAt:        last.CreatedAt.Time,
	}); err != nil {
		slog.Error("failed to store the rolling summary", "err", err, "session_id", sessionID)
		return
	}

	slog.Info("rolling summary updated", "session_id", sessionID, "messages_covered", included)
}

// summaryInputText flattens messages into a bounded transcript so a single huge
// message cannot inflate the prompt. It returns the transcript and how many of
// the leading messages made it in: only those may be recorded as summarised.
func summaryInputText(messages []openai.ChatCompletionMessage) (string, int) {
	var b strings.Builder
	used := 0
	included := 0

	for _, m := range messages {
		remaining := maxSummaryInputTokens - used
		if remaining <= 0 {
			break
		}

		line := string(m.Role) + ": " + truncateTokens(m.Content, maxSummaryMessageTokens)
		line = truncateTokens(line, remaining)

		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		used += estimateTokens(line)
		included++
	}

	return b.String(), included
}
