package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummaryInputTextIsBounded(t *testing.T) {
	olds := make([]openai.ChatCompletionMessage, maxSummaryInputMessages)
	huge := strings.Repeat("a", maxSummaryMessageTokens*bytesPerToken*3)
	for i := range olds {
		olds[i] = openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: huge,
		}
	}

	text, included := summaryInputText(olds)

	// One separator per retained line is not counted against the budget.
	if n := estimateTokens(text); n > maxSummaryInputTokens+maxSummaryInputMessages {
		t.Fatalf("summary input costs %d tokens, want at most %d", n, maxSummaryInputTokens+maxSummaryInputMessages)
	}
	require.Contains(t, text, "user: ")
	require.Greater(t, included, 0)
	require.Less(t, included, len(olds), "the token budget cuts the transcript before the message limit does")
	require.Equal(t, included, strings.Count(text, "user: "), "every included message is in the text, and no other")
}

func TestSummaryInputTextCountsEveryMessageThatFits(t *testing.T) {
	olds := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "първо"},
		{Role: openai.ChatMessageRoleAssistant, Content: "второ"},
		{Role: openai.ChatMessageRoleUser, Content: "трето"},
	}

	text, included := summaryInputText(olds)

	require.Equal(t, 3, included)
	require.Equal(t, "user: първо\nassistant: второ\nuser: трето", text)

	text, included = summaryInputText(nil)
	require.Equal(t, 0, included)
	require.Empty(t, text)
}

func numberedHistory(n int) []openai.ChatCompletionMessage {
	history := make([]openai.ChatCompletionMessage, n)
	for i := range history {
		history[i] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("съобщение %d", i)}
	}
	return history
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

	kept := pruneHistory(history, defaultHistoryTokens, defaultHistoryMessages)

	if len(kept) == 0 {
		t.Fatal("expected the newest turn to be kept")
	}
	if kept[len(kept)-1].Content != history[len(history)-1].Content {
		t.Fatal("the newest message must always be kept")
	}
	require.Equal(t, history[len(history)-len(kept):], kept, "what is kept is the newest stretch of the history, in order")

	total := 0
	for _, m := range kept {
		total += estimateTokens(m.Content)
	}
	if total > defaultHistoryTokens {
		t.Fatalf("kept history costs %d tokens, want at most %d", total, defaultHistoryTokens)
	}
}

func TestPruneHistoryHonoursTheMessageLimit(t *testing.T) {
	history := numberedHistory(30)

	kept := pruneHistory(history, 1_000_000, 7)

	require.Equal(t, history[23:], kept)
}

func TestPruneHistoryKeepsAHugeNewestMessage(t *testing.T) {
	history := numberedHistory(3)
	history[2].Content = strings.Repeat("д", 20_000)

	kept := pruneHistory(history, 100, 10)

	require.Equal(t, history[2:], kept, "the question being answered is never dropped, however large")
}

func TestWindowStartPointsAtTheOldestKeptMessage(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	history := numberedHistory(5)
	at := make([]time.Time, len(history))
	for i := range at {
		at[i] = base.Add(time.Duration(i) * time.Minute)
	}
	c := chatContext{History: history, HistoryAt: at}

	require.Equal(t, at[2], c.windowStart(3), "three kept messages start at the third from the end")
	require.Equal(t, at[0], c.windowStart(5))
	require.Equal(t, at[4], c.windowStart(1))

	require.True(t, c.windowStart(0).IsZero())
	require.True(t, c.windowStart(6).IsZero(), "more kept messages than history is a caller bug, not a position")
	require.True(t, chatContext{History: history}.windowStart(2).IsZero(), "no timestamps, no position")
}

// timedHistory is n numbered messages one minute apart, starting at minute `from`.
func timedHistory(from, n int) ([]openai.ChatCompletionMessage, []time.Time) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	history := make([]openai.ChatCompletionMessage, n)
	at := make([]time.Time, n)
	for i := range history {
		history[i] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("съобщение %d", from+i)}
		at[i] = base.Add(time.Duration(from+i) * time.Minute)
	}
	return history, at
}

// A long chat, two messages (a question and its answer) per turn, read the way the
// loader does: only the newest `limit` messages. Counts how often the first message of
// the prompt changes - every change means the model server reads the history again.
func startChanges(t *testing.T, limit, turns int, steady bool) int {
	t.Helper()
	var prev, last time.Time
	hasPrev, changes := false, 0
	for turn := 0; turn < turns; turn++ {
		total := 2*turn + 1 // the question of this turn is the newest message
		from := max(0, total-limit)
		history, at := timedHistory(from, total-from)

		var kept []openai.ChatCompletionMessage
		var start time.Time
		if steady {
			kept, start = pruneHistorySteady(history, at, prev, hasPrev, 1_000_000, limit)
		} else {
			kept = pruneHistory(history, 1_000_000, limit)
			start = at[len(at)-len(kept)]
		}
		require.LessOrEqual(t, len(kept), limit, "turn %d", turn)
		require.Equal(t, history[len(history)-len(kept):], kept, "turn %d: the kept messages are the newest stretch", turn)
		require.Equal(t, history[len(history)-1], kept[len(kept)-1], "turn %d: the newest message is kept", turn)

		if !last.IsZero() && !start.Equal(last) {
			changes++
		}
		prev, hasPrev, last = start, true, start
	}
	return changes
}

// The point of P51: once the chat is longer than the window, the sliding window moves
// its start on every turn; the steady one cuts back in steps and keeps the start between.
func TestSteadyWindowMovesItsStartRarely(t *testing.T) {
	const limit, turns = 20, 60

	sliding := startChanges(t, limit, turns, false)
	steady := startChanges(t, limit, turns, true)

	assert.Greater(t, sliding, 40, "the sliding window changes its start nearly every turn once the chat is long")
	assert.LessOrEqual(t, steady, 12, "the steady window cuts every few turns")
	assert.Less(t, steady, sliding/3)
}

func TestSteadyWindowWithoutAPreviousStartIsTheNewestThatFit(t *testing.T) {
	history, at := timedHistory(0, 30)

	kept, start := pruneHistorySteady(history, at, time.Time{}, false, 1_000_000, 12)

	require.Equal(t, pruneHistory(history, 1_000_000, 12), kept)
	require.Equal(t, at[18], start)
}

func TestSteadyWindowKeepsItsStartWhileItFits(t *testing.T) {
	history, at := timedHistory(0, 30)

	kept, start := pruneHistorySteady(history, at, at[20], true, 1_000_000, 12)

	require.Equal(t, history[20:], kept, "from the remembered start to the newest, however the newest limit slides")
	require.Equal(t, at[20], start)
}

func TestSteadyWindowIsCutBackWhenItOutgrowsTheMessageLimit(t *testing.T) {
	history, at := timedHistory(0, 30)

	kept, start := pruneHistorySteady(history, at, at[15], true, 1_000_000, 10)

	require.Len(t, kept, 6, "60 percent of 10 messages")
	require.Equal(t, history[24:], kept)
	require.Equal(t, at[24], start)
}

// The loader reads only the newest messages: a remembered start older than all of
// them cannot be kept, or the window would slide again with the loader's own window.
func TestSteadyWindowIsCutWhenItsStartIsOlderThanWhatWasLoaded(t *testing.T) {
	history, at := timedHistory(100, 10)
	older := at[0].Add(-time.Hour)

	kept, start := pruneHistorySteady(history, at, older, true, 1_000_000, 10)

	require.Len(t, kept, 6)
	require.Equal(t, at[4], start)
}

func TestSteadyWindowIsCutWhenItOutgrowsTheTokenBudget(t *testing.T) {
	history, at := timedHistory(0, 10)
	for i := range history {
		history[i].Content = strings.Repeat("д", 200) // 400 bytes, about 100 tokens
	}
	per := estimateTokens(history[0].Content)

	kept, _ := pruneHistorySteady(history, at, at[0], true, per*10-1, 100)
	require.Less(t, len(kept), 10)
	require.LessOrEqual(t, len(kept)*per, (per*10-1)*historyCutPercent/100+per, "cut to 60 percent of the budget")

	kept, start := pruneHistorySteady(history, at, at[0], true, per*10, 100)
	require.Len(t, kept, 10, "exactly the budget still fits")
	require.Equal(t, at[0], start)
}

func TestSteadyWindowKeepsAHugeNewestMessage(t *testing.T) {
	history, at := timedHistory(0, 3)
	history[2].Content = strings.Repeat("д", 20_000)

	kept, start := pruneHistorySteady(history, at, at[2], true, 100, 10)

	require.Equal(t, history[2:], kept)
	require.Equal(t, at[2], start)
}

func TestSteadyWindowWithoutTimesIsPlainPruning(t *testing.T) {
	history := numberedHistory(30)

	kept, start := pruneHistorySteady(history, nil, time.Now(), true, 1_000_000, 7)

	require.Equal(t, pruneHistory(history, 1_000_000, 7), kept)
	require.True(t, start.IsZero())
}

func TestHistoryWindowsRememberAndForget(t *testing.T) {
	var w historyWindows
	_, ok := w.get("a")
	require.False(t, ok)

	first := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	w.put("a", first)
	got, ok := w.get("a")
	require.True(t, ok)
	require.Equal(t, first, got)

	for i := 0; i < maxTrackedWindows; i++ {
		w.put(fmt.Sprintf("k%d", i), first)
	}
	_, ok = w.get("a")
	require.False(t, ok, "the oldest session is forgotten when the table is full")
	require.Len(t, w.start, maxTrackedWindows)
	_, ok = w.get(fmt.Sprintf("k%d", maxTrackedWindows-1))
	require.True(t, ok)
}

func TestSummaryRunsAllowsOnePassPerSession(t *testing.T) {
	var runs summaryRuns
	keyA := summaryKey(uuid.New(), "same-session")
	keyB := summaryKey(uuid.New(), "same-session")

	require.True(t, runs.begin(keyA))
	require.False(t, runs.begin(keyA), "a second pass for the same session must not start")
	require.True(t, runs.begin(keyB), "the same session id of another user is a different session")

	runs.end(keyA)
	require.True(t, runs.begin(keyA), "once the pass ended, the session can be summarised again")
	assert.Len(t, runs.running, 2)
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
