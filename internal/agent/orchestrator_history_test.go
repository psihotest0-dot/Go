package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
)

func TestSessionSummaryStoreEvictsIdleSessions(t *testing.T) {
	store := newSessionSummaryStore()

	stale := store.get("sess-stale")
	stale.touch(time.Now().Add(-2 * sessionSummaryTTL))
	store.get("sess-fresh")
	require.Equal(t, 2, store.size())

	// The periodic sweep is rate limited, so make it due again before
	// triggering it through a lookup.
	forceSweepDue(store)
	store.get("sess-new")

	_, ok := store.peek("sess-stale")
	require.False(t, ok, "idle session should have been evicted")
	_, ok = store.peek("sess-fresh")
	require.True(t, ok, "recently used session must be kept")
	require.Equal(t, 2, store.size(), "cache should only hold the live sessions")
}

// forceSweepDue resets the sweep throttle so the next lookup performs a full
// eviction pass.
func forceSweepDue(store *sessionSummaryStore) {
	store.mu.Lock()
	store.lastSweep = time.Time{}
	store.mu.Unlock()
}

func TestSessionSummaryStoreEnforcesCap(t *testing.T) {
	store := newSessionSummaryStore()

	for i := 0; i < maxSessionSummaries; i++ {
		state := store.get(fmt.Sprintf("sess-%d", i))
		// Age the entries so the least recently used one is deterministic.
		// They stay within the TTL, so only the hard cap can evict them.
		state.touch(time.Now().Add(-time.Duration(maxSessionSummaries-i) * time.Second))
	}
	require.Equal(t, maxSessionSummaries, store.size())

	store.get("sess-new")

	require.Equal(t, maxSessionSummaries, store.size(), "cache must stay at its cap")
	_, ok := store.peek("sess-0")
	require.False(t, ok, "least recently used session should be evicted")
	_, ok = store.peek("sess-new")
	require.True(t, ok, "new session should be cached")
}

func TestSessionSummaryStoreKeepsInFlightSessions(t *testing.T) {
	store := newSessionSummaryStore()

	inFlight := store.get("sess-inflight")
	inFlight.isGenerating.Store(true)
	inFlight.touch(time.Now().Add(-2 * sessionSummaryTTL))

	forceSweepDue(store)
	store.get("sess-other")

	_, ok := store.peek("sess-inflight")
	require.True(t, ok, "a session with a summary in flight must not be evicted")
}

func TestSummaryInputTextIsBounded(t *testing.T) {
	olds := make([]openai.ChatCompletionMessage, maxSummaryInputMessages)
	huge := strings.Repeat("a", maxSummaryMessageTokens*bytesPerToken*3)
	for i := range olds {
		olds[i] = openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: huge,
		}
	}

	text := summaryInputText(olds)

	// One separator per retained line is not counted against the budget.
	if n := estimateTokens(text); n > maxSummaryInputTokens+maxSummaryInputMessages {
		t.Fatalf("summary input costs %d tokens, want at most %d", n, maxSummaryInputTokens+maxSummaryInputMessages)
	}
	require.Contains(t, text, "user: ")
}

// TestPruneHistoryBoundsTokens covers the history replayed on every turn: the
// budget is charged in estimated tokens, so Cyrillic history cannot grow to
// twice the intended context, and the newest turn is always kept.
func TestPruneHistoryBoundsTokens(t *testing.T) {
	history := make([]openai.ChatCompletionMessage, 0, 20)
	for i := 0; i < 20; i++ {
		history = append(history, openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleUser,
			// 1990 Cyrillic characters are 3980 bytes, so every turn costs
			// roughly a thousand tokens.
			Content: fmt.Sprintf("%s-%d", strings.Repeat("д", 1990), i),
		})
	}

	kept, dropped, droppedCount := pruneHistory(history)

	if len(kept) == 0 {
		t.Fatal("expected the newest turn to be kept")
	}
	if kept[len(kept)-1].Content != history[len(history)-1].Content {
		t.Fatal("the newest message must always be kept")
	}
	if len(kept)+len(dropped) != len(history) {
		t.Fatalf("kept %d + dropped %d messages, want %d", len(kept), len(dropped), len(history))
	}
	if droppedCount != len(dropped) {
		t.Fatalf("droppedCount = %d, want %d", droppedCount, len(dropped))
	}

	total := 0
	for _, m := range kept {
		total += estimateTokens(m.Content)
	}
	if total > maxContextTokens {
		t.Fatalf("kept history costs %d tokens, want at most %d", total, maxContextTokens)
	}
}

func TestRollingSummaryGeneratesSummaryInBackground(t *testing.T) {
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	mock := &llm.MockLLM{
		Response: openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{
				{
					Message: openai.ChatCompletionMessage{
						Role:    openai.ChatMessageRoleAssistant,
						Content: "  summarised conversation  ",
					},
				},
			},
		},
	}
	orchestrator := NewOrchestrator(nil, nil, mock, nil)

	olds := make([]openai.ChatCompletionMessage, summaryMinNewMessages)
	for i := range olds {
		olds[i] = openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: "old message",
		}
	}

	userID := uuid.New()

	// Below the threshold no summary is requested.
	require.Equal(t, "", orchestrator.rollingSummary(userID, "sess-1", olds, summaryMinNewMessages-1))
	require.Equal(t, 1, orchestrator.summaries.size())

	require.Equal(t, "", orchestrator.rollingSummary(userID, "sess-1", olds, summaryMinNewMessages))

	// The summary is produced asynchronously.
	require.Eventually(t, func() bool {
		return orchestrator.rollingSummary(userID, "sess-1", olds, summaryMinNewMessages) == "summarised conversation"
	}, 5*time.Second, 10*time.Millisecond)

	state, ok := orchestrator.summaries.peek(summaryKey(userID, "sess-1"))
	require.True(t, ok)
	require.False(t, state.isGenerating.Load(), "the in-flight marker must be cleared")
	require.LessOrEqual(t, estimateTokens(state.text), maxSummaryTokens)
}

func TestRollingSummarySurvivesSummariserFailure(t *testing.T) {
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	orchestrator := NewOrchestrator(nil, nil, &llm.MockLLM{Error: fmt.Errorf("llm unavailable")}, nil)

	olds := make([]openai.ChatCompletionMessage, summaryMinNewMessages)
	for i := range olds {
		olds[i] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "old message"}
	}

	userID := uuid.New()

	require.Equal(t, "", orchestrator.rollingSummary(userID, "sess-2", olds, summaryMinNewMessages))

	// A failed summarisation must not leave the session permanently marked as
	// in flight, otherwise it would never be summarised again.
	require.Eventually(t, func() bool {
		state, ok := orchestrator.summaries.peek(summaryKey(userID, "sess-2"))
		return ok && !state.isGenerating.Load()
	}, 5*time.Second, 10*time.Millisecond)

	state, _ := orchestrator.summaries.peek(summaryKey(userID, "sess-2"))
	require.Equal(t, "", state.text)
	require.Equal(t, 0, state.lastDropCount)
}

// TestRollingSummaryIsScopedPerUser guards the cache key: session IDs are
// supplied by the client, so two users that pick the same session ID must not
// share (or overwrite) each other's rolling summary.
func TestRollingSummaryIsScopedPerUser(t *testing.T) {
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	mock := &llm.MockLLM{
		Response: openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{
				{
					Message: openai.ChatCompletionMessage{
						Role:    openai.ChatMessageRoleAssistant,
						Content: "user A private summary",
					},
				},
			},
		},
	}
	orchestrator := NewOrchestrator(nil, nil, mock, nil)

	olds := make([]openai.ChatCompletionMessage, summaryMinNewMessages)
	for i := range olds {
		olds[i] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "old message"}
	}

	userA := uuid.New()
	userB := uuid.New()

	require.Equal(t, "", orchestrator.rollingSummary(userA, "shared-session", olds, summaryMinNewMessages))
	require.Eventually(t, func() bool {
		return orchestrator.rollingSummary(userA, "shared-session", olds, summaryMinNewMessages) == "user A private summary"
	}, 5*time.Second, 10*time.Millisecond)

	// Same session ID, different owner: user B sees nothing of user A's summary.
	require.Equal(t, "", orchestrator.rollingSummary(userB, "shared-session", olds, 0))
	require.Equal(t, 2, orchestrator.summaries.size(), "each user needs its own cache entry")

	stateA, ok := orchestrator.summaries.peek(summaryKey(userA, "shared-session"))
	require.True(t, ok)
	require.Equal(t, "user A private summary", stateA.text)

	stateB, ok := orchestrator.summaries.peek(summaryKey(userB, "shared-session"))
	require.True(t, ok)
	require.Equal(t, "", stateB.text)
}

func TestDropTrailingAssistantMessages(t *testing.T) {
	user := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "question"}
	assistant := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "answer"}

	// The reply being regenerated must not be part of the prompt.
	require.Equal(t, []openai.ChatCompletionMessage{user}, dropTrailingAssistantMessages([]openai.ChatCompletionMessage{user, assistant}))

	// Consecutive trailing replies are all removed.
	require.Equal(t, []openai.ChatCompletionMessage{user}, dropTrailingAssistantMessages([]openai.ChatCompletionMessage{user, assistant, assistant}))

	// A history that already ends with a user message is left untouched.
	history := []openai.ChatCompletionMessage{assistant, user}
	require.Equal(t, history, dropTrailingAssistantMessages(history))

	require.Empty(t, dropTrailingAssistantMessages(nil))
}
