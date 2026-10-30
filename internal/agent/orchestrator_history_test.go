package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	huge := strings.Repeat("a", maxSummaryMessageChars*3)
	for i := range olds {
		olds[i] = openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: huge,
		}
	}

	text := summaryInputText(olds)

	// One separator per retained line is not counted against the budget.
	require.LessOrEqual(t, utf8.RuneCountInString(text), maxSummaryInputChars+maxSummaryInputMessages)
	require.Contains(t, text, "user: ")
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

	// Below the threshold no summary is requested.
	require.Equal(t, "", orchestrator.rollingSummary("sess-1", olds, summaryMinNewMessages-1))
	require.Equal(t, 1, orchestrator.summaries.size())

	require.Equal(t, "", orchestrator.rollingSummary("sess-1", olds, summaryMinNewMessages))

	// The summary is produced asynchronously.
	require.Eventually(t, func() bool {
		return orchestrator.rollingSummary("sess-1", olds, summaryMinNewMessages) == "summarised conversation"
	}, 5*time.Second, 10*time.Millisecond)

	state, ok := orchestrator.summaries.peek("sess-1")
	require.True(t, ok)
	require.False(t, state.isGenerating.Load(), "the in-flight marker must be cleared")
	require.LessOrEqual(t, utf8.RuneCountInString(state.text), maxSummaryChars+3)
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

	require.Equal(t, "", orchestrator.rollingSummary("sess-2", olds, summaryMinNewMessages))

	// A failed summarisation must not leave the session permanently marked as
	// in flight, otherwise it would never be summarised again.
	require.Eventually(t, func() bool {
		state, ok := orchestrator.summaries.peek("sess-2")
		return ok && !state.isGenerating.Load()
	}, 5*time.Second, 10*time.Millisecond)

	state, _ := orchestrator.summaries.peek("sess-2")
	require.Equal(t, "", state.text)
	require.Equal(t, 0, state.lastDropCount)
}
