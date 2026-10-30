package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/prompts"
)

const (
	// summaryMinNewMessages is how many newly pruned messages must accumulate
	// before a rolling summary is (re)generated.
	summaryMinNewMessages = 6
	// summaryTimeout bounds a single summarisation LLM call.
	summaryTimeout = 2 * time.Minute
	// maxSummaryTokens caps the retained summary so a runaway model response
	// cannot be pinned in memory for the lifetime of a session, and so the
	// summary cannot push the system prompt past the context window.
	maxSummaryTokens = 1000
	// maxSummaryInputMessages, maxSummaryInputTokens and
	// maxSummaryMessageTokens bound what is handed to the summariser and
	// therefore what its background goroutine keeps alive.
	maxSummaryInputMessages = 40
	maxSummaryInputTokens   = 3000
	maxSummaryMessageTokens = 500

	// The rolling summary cache is bounded: session IDs are client supplied, so
	// idle entries expire after sessionSummaryTTL and the cache never holds
	// more than maxSessionSummaries entries.
	maxSessionSummaries         = 1024
	sessionSummaryTTL           = 30 * time.Minute
	sessionSummarySweepInterval = time.Minute
)

// summaryKey scopes a cached rolling summary to its owner. Session IDs are
// client supplied, so keying the cache by session alone would let two users who
// pick the same session ID read each other's summary.
func summaryKey(userID uuid.UUID, sessionID string) string {
	return userID.String() + "\x00" + sessionID
}

// sessionSummaryStore is a TTL- and size-bounded cache of rolling summaries.
// Use newSessionSummaryStore to create one.
type sessionSummaryStore struct {
	mu        sync.Mutex
	entries   map[string]*sessionSummary
	lastSweep time.Time
}

func newSessionSummaryStore() *sessionSummaryStore {
	return &sessionSummaryStore{entries: make(map[string]*sessionSummary)}
}

// get returns the summary state for key, creating it when missing.
// Idle and surplus entries are evicted opportunistically, so the cache cannot
// grow without bound.
func (s *sessionSummaryStore) get(key string) *sessionSummary {
	if s == nil {
		// A zero-value Orchestrator has no cache: return a throwaway state so
		// the call still works but nothing is retained.
		return &sessionSummary{}
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if state, ok := s.entries[key]; ok {
		state.touch(now)
		return state
	}

	s.evictLocked(now)

	state := &sessionSummary{}
	state.touch(now)
	s.entries[key] = state
	return state
}

// peek returns the state for key without creating or touching it.
func (s *sessionSummaryStore) peek(key string) (*sessionSummary, bool) {
	if s == nil {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.entries[key]
	return state, ok
}

// size reports how many summaries are currently cached.
func (s *sessionSummaryStore) size() int {
	if s == nil {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.entries)
}

// evictLocked drops expired entries and enforces the hard cap. Callers must
// hold s.mu. Entries with a summarisation in flight are skipped so their result
// is not discarded; a later sweep reclaims them.
func (s *sessionSummaryStore) evictLocked(now time.Time) {
	sweepDue := s.lastSweep.IsZero() || now.Sub(s.lastSweep) >= sessionSummarySweepInterval
	if !sweepDue && len(s.entries) < maxSessionSummaries {
		return
	}
	s.lastSweep = now

	for id, state := range s.entries {
		if state.isGenerating.Load() {
			continue
		}
		if now.Sub(state.lastAccessed()) > sessionSummaryTTL {
			delete(s.entries, id)
		}
	}

	// Enforce the hard cap by dropping the least recently used entries.
	for len(s.entries) >= maxSessionSummaries {
		victim := ""
		var oldest int64
		for id, state := range s.entries {
			if state.isGenerating.Load() {
				continue
			}
			at := state.lastUsed.Load()
			if victim == "" || at < oldest {
				victim, oldest = id, at
			}
		}
		if victim == "" {
			// Every cached session is mid-summarisation, leave them alone.
			return
		}
		delete(s.entries, victim)
	}
}

// dropTrailingAssistantMessages removes assistant replies from the end of the
// history. Regenerating keeps the previous answer in the database until a new
// one is ready, so it has to be dropped from the prompt: otherwise the model is
// asked to answer a question it has already answered, with that answer in view.
func dropTrailingAssistantMessages(historyMessages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	end := len(historyMessages)
	for end > 0 && historyMessages[end-1].Role == openai.ChatMessageRoleAssistant {
		end--
	}
	if end == len(historyMessages) {
		return historyMessages
	}
	return historyMessages[:end]
}

func pruneHistory(historyMessages []openai.ChatCompletionMessage) ([]openai.ChatCompletionMessage, []openai.ChatCompletionMessage, int) {
	var oldMessages []openai.ChatCompletionMessage
	var reversedKeep []openai.ChatCompletionMessage
	currentTokens := 0

	for i := len(historyMessages) - 1; i >= 0; i-- {
		msgTokens := estimateTokens(historyMessages[i].Content)

		if i != len(historyMessages)-1 {
			if (currentTokens+msgTokens > maxContextTokens && len(reversedKeep) > 0) || len(reversedKeep) >= maxHistoryMessages {
				oldMessages = historyMessages[:i+1]
				break
			}
		}
		reversedKeep = append(reversedKeep, historyMessages[i])
		currentTokens += msgTokens
	}

	var finalHistory []openai.ChatCompletionMessage
	for i := len(reversedKeep) - 1; i >= 0; i-- {
		finalHistory = append(finalHistory, reversedKeep[i])
	}

	droppedMessagesCount := len(oldMessages)
	return finalHistory, oldMessages, droppedMessagesCount
}

// rollingSummary returns the cached summary for the user's session and, once
// enough new history has been pruned, refreshes it in the background.
func (o *Orchestrator) rollingSummary(userID uuid.UUID, sessionID string, oldMessages []openai.ChatCompletionMessage, droppedMessagesCount int) string {
	key := summaryKey(userID, sessionID)
	state := o.summaries.get(key)

	state.mu.Lock()
	summary := state.text
	lastDropCount := state.lastDropCount
	state.mu.Unlock()

	if droppedMessagesCount <= 0 || droppedMessagesCount-lastDropCount < summaryMinNewMessages {
		return summary
	}
	if !state.isGenerating.CompareAndSwap(false, true) {
		// A summarisation for this session is already in flight.
		return summary
	}

	var newOlds []openai.ChatCompletionMessage
	if droppedMessagesCount > lastDropCount && lastDropCount < len(oldMessages) {
		newOlds = oldMessages[lastDropCount:]
	}
	if len(newOlds) > maxSummaryInputMessages {
		newOlds = newOlds[len(newOlds)-maxSummaryInputMessages:]
	}
	if len(newOlds) == 0 {
		state.isGenerating.Store(false)
		return summary
	}

	// Copy the transcript so the background goroutine does not keep the whole
	// request history (chatCtx.History) alive.
	olds := make([]openai.ChatCompletionMessage, len(newOlds))
	copy(olds, newOlds)

	go o.generateSummary(key, userID, sessionID, olds, droppedMessagesCount, summary)

	return summary
}

// generateSummary renders the rolling summary prompt and asks the LLM for an
// updated summary. It runs in a detached goroutine, so it must never panic and
// must always clear the in-flight marker.
func (o *Orchestrator) generateSummary(key string, userID uuid.UUID, sessionID string, olds []openai.ChatCompletionMessage, droppedCount int, previousSummary string) {
	defer func() {
		if rcv := recover(); rcv != nil {
			slog.Error("panic while generating rolling summary", "panic", rcv, "session_id", sessionID, "user_id", userID)
		}
		if state, ok := o.summaries.peek(key); ok {
			state.isGenerating.Store(false)
		}
	}()

	if len(olds) == 0 {
		return
	}
	if prompts.GlobalManager == nil {
		slog.Error("prompt manager not initialised, skipping rolling summary", "session_id", sessionID)
		return
	}

	summaryPrompt, err := prompts.GlobalManager.Render("rolling_summary.tmpl", map[string]interface{}{
		"PreviousSummary": previousSummary,
		"MessagesText":    summaryInputText(olds),
	})
	if err != nil {
		slog.Error("failed to render rolling summary prompt", "err", err)
		return
	}

	summaryReq := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: summaryPrompt,
			},
		},
		MaxTokens:   300,
		Temperature: 0.3,
	}

	sumCtx, sumCancel := context.WithTimeout(context.Background(), summaryTimeout)
	defer sumCancel()

	summaryResp, sumErr := o.llmClient.Chat(sumCtx, summaryReq)
	if sumErr != nil {
		slog.Error("failed to generate rolling summary", "err", sumErr)
		return
	}
	if len(summaryResp.Choices) == 0 {
		slog.Error("rolling summary response contained no choices", "session_id", sessionID)
		return
	}

	state, ok := o.summaries.peek(key)
	if !ok {
		// The session was evicted meanwhile, drop the result.
		return
	}
	state.mu.Lock()
	state.text = truncateTokens(strings.TrimSpace(summaryResp.Choices[0].Message.Content), maxSummaryTokens)
	state.lastDropCount = droppedCount
	state.mu.Unlock()
}

// summaryInputText flattens pruned messages into a bounded transcript so a
// single huge message cannot inflate the prompt or the memory held by the
// background goroutine.
func summaryInputText(olds []openai.ChatCompletionMessage) string {
	var b strings.Builder
	used := 0

	for _, m := range olds {
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
	}

	return b.String()
}
