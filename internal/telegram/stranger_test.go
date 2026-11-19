package telegram

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForTurns(t *testing.T, b *Bot) {
	t.Helper()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("turns are still running")
	}
}

// What a stranger costs must not grow with how much they write: other text gets no
// goroutine (nothing to wait for, nothing sent, nothing touched).
func TestStrangersTextIsDroppedWithoutAGoroutine(t *testing.T) {
	f := newFixture(42)
	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for i := 0; i < 50; i++ {
		for _, text := range []string{"Здравей", "/pending", "/link 123"} {
			release, ok := f.bot.admitStranger(privateMessage(999, text))
			assert.False(t, ok, "%q is not admitted", text)
			assert.Nil(t, release)
		}
		f.bot.dispatch(context.Background(), privateMessage(999, "Здравей"))
	}
	waitForTurns(t, f.bot)

	assert.Empty(t, f.sender.texts())
	assert.Zero(t, f.processor.calls.Load())
	assert.Empty(t, f.bot.chatTails)
	assert.Equal(t, 1, strings.Count(logs.String(), "unauthorized chat"), "one log line per chat per minute, not one per message")
}

// /id is how the owner finds the id to allow, so it is answered - once per chat
// every few seconds, however many times it is sent.
func TestIDIsAnsweredOncePerStrangerChatEveryTenSeconds(t *testing.T) {
	f := newFixture(42)

	for i := 0; i < 5; i++ {
		f.bot.dispatch(context.Background(), privateMessage(999, "/id"))
	}
	f.bot.dispatch(context.Background(), privateMessage(998, "/ID@Lili_Smart_bot"))
	waitForTurns(t, f.bot)

	assert.ElementsMatch(t, []string{"Chat id: 999", "Chat id: 998"}, f.sender.texts())
}

// Many different chats at once cannot start more than maxStrangerReplies replies.
func TestStrangerRepliesInFlightAreBounded(t *testing.T) {
	f := newFixture(42)
	f.sender.messageDelay = 200 * time.Millisecond // keeps every accepted reply in flight

	for chat := int64(1000); chat < 1000+4*maxStrangerReplies; chat++ {
		f.bot.dispatch(context.Background(), privateMessage(chat, "/id"))
	}
	waitForTurns(t, f.bot)

	assert.Len(t, f.sender.texts(), maxStrangerReplies)
}

// The owner's own chat is not throttled: it is allowed, and /id there costs nothing.
func TestIDFromAnAllowedChatIsNotThrottled(t *testing.T) {
	f := newFixture(42)

	for i := 0; i < 3; i++ {
		f.bot.dispatch(context.Background(), privateMessage(42, "/id"))
	}
	waitForTurns(t, f.bot)

	assert.Equal(t, []string{"Chat id: 42", "Chat id: 42", "Chat id: 42"}, f.sender.texts())
}

func TestThrottleLetsOneThroughPerInterval(t *testing.T) {
	th := newThrottle(10*time.Second, 100)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	require.True(t, th.allow(1, now))
	require.False(t, th.allow(1, now.Add(9*time.Second)))
	require.True(t, th.allow(2, now.Add(9*time.Second)), "another id has its own clock")
	require.True(t, th.allow(1, now.Add(10*time.Second)), "after the interval")
	require.False(t, th.allow(1, now.Add(11*time.Second)), "the clock restarted at the last pass")
}

func TestThrottleTableIsBounded(t *testing.T) {
	th := newThrottle(10*time.Second, 3)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for id := int64(1); id <= 3; id++ {
		require.True(t, th.allow(id, now))
	}

	require.False(t, th.allow(4, now.Add(time.Second)), "full, nothing has expired: a new id is refused")
	require.Len(t, th.last, 3)
	require.True(t, th.allow(4, now.Add(11*time.Second)), "the expired entries make room")
	assert.LessOrEqual(t, len(th.last), 3, fmt.Sprint(th.last))
}
