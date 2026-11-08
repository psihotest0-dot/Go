package telegram

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/agent"
)

// waitForTyping blocks the model call (the caller's goroutine) until the bot has
// shown "typing..." n times, the way a long turn would.
func waitForTyping(f *fixture, n int) {
	deadline := time.Now().Add(3 * time.Second)
	for f.sender.typingCount() < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}

// Telegram clears "typing..." after about five seconds, and a turn with tools
// takes longer: the action has to be repeated while the model works, and it must
// not arrive after the reply (the indicator would stay on with nothing coming).
func TestTypingIsRepeatedWhileTheModelWorksAndStopsBeforeTheReply(t *testing.T) {
	f := newFixture(42)
	f.bot.typingEvery = 10 * time.Millisecond
	// A request takes longer than the interval, so one is nearly always on its
	// way when the model finishes. The reply takes longer than several
	// intervals: a tick that was not stopped would land inside it.
	f.sender.typingDelay = 30 * time.Millisecond
	f.sender.messageDelay = 80 * time.Millisecond
	shownBeforeTheModel := -1
	f.processor.onCall = func(context.Context, uuid.UUID, agent.ChatRequest) {
		shownBeforeTheModel = f.sender.typingCount()
		waitForTyping(f, 3)
	}

	f.bot.handleMessage(context.Background(), privateMessage(42, "здравей"))

	events := f.sender.eventLog()
	require.NotEmpty(t, events)
	assert.Equal(t, 1, shownBeforeTheModel, "the first one is shown at once, before the model is called")
	assert.GreaterOrEqual(t, f.sender.typingCount(), 3, "repeated, not only once at the start")
	assert.Equal(t, "message", events[len(events)-1], "nothing is shown as typing after the reply")
	assert.Equal(t, []string{"отговор"}, f.sender.texts())

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, events, f.sender.eventLog(), "the repeating has stopped for good")
}

func TestTypingStopsBeforeTheErrorReply(t *testing.T) {
	f := newFixture(42)
	f.bot.typingEvery = 10 * time.Millisecond
	f.sender.messageDelay = 60 * time.Millisecond
	f.processor.err = errors.New("model is down")
	f.processor.onCall = func(context.Context, uuid.UUID, agent.ChatRequest) { waitForTyping(f, 2) }

	f.bot.handleMessage(context.Background(), privateMessage(42, "здравей"))

	events := f.sender.eventLog()
	require.Equal(t, 1, len(f.sender.texts()), "one generic error reply")
	assert.Equal(t, "message", events[len(events)-1])
	assert.GreaterOrEqual(t, f.sender.typingCount(), 2)
}

func TestTypingStopsWhenTheBotShutsDown(t *testing.T) {
	f := newFixture(42)
	f.bot.typingEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	f.processor.err = context.Canceled
	f.processor.onCall = func(context.Context, uuid.UUID, agent.ChatRequest) {
		waitForTyping(f, 2)
		cancel()
	}

	f.bot.handleMessage(ctx, privateMessage(42, "здравей"))

	before := f.sender.typingCount()
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, before, f.sender.typingCount(), "no typing after the turn ended")
	assert.Empty(t, f.sender.texts(), "no reply while shutting down")
}

// Commands that do not call the model have nothing to wait for.
func TestNoTypingForCommandsThatDoNotCallTheModel(t *testing.T) {
	f := newFixture(42)

	f.bot.handleMessage(context.Background(), privateMessage(42, "/pending"))

	assert.Zero(t, f.sender.typingCount())
}
