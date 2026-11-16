package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/prompts"
)

// The system prompt is the start of every request, and both servers - llama-server behind
// Lemonade and DeepSeek - reuse the part of the request that is the same as the one before it
// (the cache of the prefix) and work only on what follows the first difference. The date of
// the prompt used to be on line 4, with seconds, so it was a different prefix in every turn
// and nothing after it was reused (measured on 2026-11-16: 0 of 3 709 tokens from the cache on
// the local model, 0 of 4 003 at DeepSeek, against 3 682 and 3 840 with the date later; plan
// item P50). The date now stands after the blocks that stay the same from turn to turn and
// before the ones that change with the question, and it is the hour, not the second.

func promptAt(t *testing.T, clock time.Time, c chatContext) string {
	t.Helper()
	previous, previousClock := prompts.GlobalManager, promptClock
	prompts.Init("")
	promptClock = func() time.Time { return clock }
	t.Cleanup(func() { prompts.GlobalManager, promptClock = previous, previousClock })
	msg, err := buildSystemPrompt(c)
	require.NoError(t, err)
	return msg.Content
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func TestThePromptDateIsTheHourAndStandsBetweenTheStableAndTheChangingBlocks(t *testing.T) {
	c := chatContext{
		StyleNotes: []string{"СТИЛ-БЕЛЕЖКА"}, TopMemories: []string{"ПРОФИЛЕН-ФАКТ"},
		RollingSum: "РЕЗЮМЕ-X", Memories: []string{"СПОМЕН-Y"}, Chunks: []string{"ОТКЪС-Z"},
	}
	out := promptAt(t, time.Date(2026, 11, 16, 16, 47, 33, 0, time.Local), c)

	assert.Contains(t, out, "2026-11-16 16:00", "the hour, not the minute and the second")
	assert.NotContains(t, out, "16:47", "no minutes")
	assert.NotContains(t, out, "16:47:33")
	date := strings.Index(out, "Current date")
	require.GreaterOrEqual(t, date, 0)
	for _, stable := range []string{"EXAMPLE TONE", "СТИЛ-БЕЛЕЖКА", "ПРОФИЛЕН-ФАКТ"} {
		assert.Less(t, strings.Index(out, stable), date, "%q is before the date: it does not change from turn to turn", stable)
	}
	for _, changing := range []string{"РЕЗЮМЕ-X", "СПОМЕН-Y", "ОТКЪС-Z", "REPLY RULES"} {
		assert.Greater(t, strings.Index(out, changing), date, "%q is after the date: it may change with the question", changing)
	}
}

// What is reused is the common beginning of two requests. Two turns of one conversation, a
// few minutes apart, with other memories and passages found for the question, share the
// whole beginning up to the changing blocks: the character, the rules, the examples, the
// notes on how we talk and the profile.
func TestTwoTurnsOfAConversationShareTheBeginningOfThePrompt(t *testing.T) {
	stable := chatContext{StyleNotes: []string{"СТИЛ-БЕЛЕЖКА"}, TopMemories: []string{"ПРОФИЛЕН-ФАКТ"}}
	first := stable
	first.Memories, first.Chunks = []string{"СПОМЕН-ЕДНО"}, []string{"ОТКЪС-ЕДНО"}
	second := stable
	second.Memories, second.Chunks = []string{"СПОМЕН-ДВЕ"}, []string{"ОТКЪС-ДВЕ"}

	a := promptAt(t, time.Date(2026, 11, 16, 16, 5, 1, 0, time.Local), first)
	b := promptAt(t, time.Date(2026, 11, 16, 16, 58, 40, 0, time.Local), second)

	shared := commonPrefix(a, b)
	assert.Greater(t, shared, strings.Index(a, "ПРОФИЛЕН-ФАКТ"), "the profile is inside the shared beginning")
	assert.Greater(t, shared, strings.Index(a, "Current date"), "so is the date: it is the same hour")
	assert.Greater(t, shared*100/len(a), 70, "most of the prompt is the shared beginning, %d of %d bytes", shared, len(a))

	// Two turns with nothing different but the clock inside the hour are one prompt.
	c := promptAt(t, time.Date(2026, 11, 16, 16, 59, 59, 0, time.Local), first)
	assert.Equal(t, a, c)

	// The next hour is a new date, after the beginning.
	d := promptAt(t, time.Date(2026, 11, 16, 17, 0, 0, 0, time.Local), first)
	assert.NotEqual(t, a, d)
	assert.Greater(t, commonPrefix(a, d), strings.Index(a, "ПРОФИЛЕН-ФАКТ"), "a new hour changes nothing before the date")
}
