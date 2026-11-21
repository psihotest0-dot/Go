package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The answers given in the app to a conversation shared with Telegram are repeated in the chat.

type mirrored struct {
	userID      uuid.UUID
	session     string
	question    string
	answer      string
	regenerated bool
	hadDeadline bool
	heldLock    bool
}

// recorder is a ReplyMirror that remembers what it was told and whether the conversation was
// still held at that moment (a second turn of it cannot get in).
type recorder struct {
	mu    sync.Mutex
	calls []mirrored
	f     *sanitizeFixture
}

func (r *recorder) mirror(ctx context.Context, userID uuid.UUID, session, question, answer string, regenerated bool) {
	_, hasDeadline := ctx.Deadline()
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	unlock, err := r.f.orchestrator.sessions.lock(short, userID, session, nil)
	if err == nil {
		unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, mirrored{userID, session, question, answer, regenerated, hasDeadline, err != nil})
}

func (r *recorder) all() []mirrored {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mirrored(nil), r.calls...)
}

func newMirrorFixture(t *testing.T, chunks ...string) (*sanitizeFixture, *recorder) {
	t.Helper()
	f := newSanitizeFixture(t, chunks...)
	rec := &recorder{f: f}
	f.orchestrator.SetReplyMirror(rec.mirror)
	return f, rec
}

func TestAnAnswerInTheAppToTheTelegramConversationIsRepeated(t *testing.T) {
	f, rec := newMirrorFixture(t, "Здравей, ", "свят")
	w := newLiveWriter()
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Какво правиш?"})

	f.orchestrator.HandleStreamChat(w, req)

	require.Len(t, rec.all(), 1)
	got := rec.all()[0]
	assert.Equal(t, f.userID, got.userID)
	assert.Equal(t, "tg-42", got.session)
	assert.Equal(t, "Какво правиш?", got.question)
	assert.Equal(t, "Здравей, свят", got.answer)
	assert.False(t, got.regenerated)
	assert.True(t, got.hadDeadline, "a slow Telegram must not hold the conversation for ever")
	assert.True(t, got.heldLock, "the repeat comes before the next turn of the conversation")
	assert.Contains(t, w.body(), "data: [DONE]")
}

func TestWhatTheModelOnlyThoughtIsNotRepeated(t *testing.T) {
	f, rec := newMirrorFixture(t, "<think>мисля </think>", "")
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})

	f.orchestrator.HandleStreamChat(newLiveWriter(), req)

	assert.Empty(t, rec.all())
}

func TestTheThinkingOfTheModelIsNotRepeated(t *testing.T) {
	f, rec := newMirrorFixture(t, "<think>скрито</think>\n\nВидим ", "отговор")
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})

	f.orchestrator.HandleStreamChat(newLiveWriter(), req)

	require.Len(t, rec.all(), 1)
	assert.Equal(t, "Видим отговор", rec.all()[0].answer)
}

func TestAnOrdinaryConversationIsNotRepeated(t *testing.T) {
	f, rec := newMirrorFixture(t, "Отговор")
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "5f3a9c1e-7b2d-4c8a-9e61-0d4b7a2c8f13", Content: "Здравей"})

	f.orchestrator.HandleStreamChat(newLiveWriter(), req)

	assert.Empty(t, rec.all())
}

func TestAFailedAnswerIsNotRepeated(t *testing.T) {
	f, rec := newMirrorFixture(t, "Отговор")
	f.local().Error = errors.New("model down")
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})

	f.orchestrator.HandleStreamChat(newLiveWriter(), req)

	assert.Empty(t, rec.all())
}

func TestAClientThatLeftBeforeTheAnswerWasCompleteGetsNoRepeat(t *testing.T) {
	f, rec := newMirrorFixture(t, "Отговор")
	gate := f.gateFirstCall(t)
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})
	ctx, cancel := context.WithCancel(req.Context())
	done := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req.WithContext(ctx))
	gate.waitUntilGenerating(t)
	cancel()
	waitFor(t, done, "the turn of the client that left")

	assert.Empty(t, rec.all(), "an answer that was never stored is not repeated")
}

func TestARegeneratedAnswerIsRepeatedWithItsQuestion(t *testing.T) {
	f, rec := newMirrorFixture(t, "Нов отговор")
	f.seedTurn(t, "tg-42", "Стар въпрос", "Стар отговор")
	_, req := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "tg-42"})

	f.orchestrator.HandleRegenerate(newLiveWriter(), req)

	require.Len(t, rec.all(), 1)
	got := rec.all()[0]
	assert.Equal(t, "Стар въпрос", got.question)
	assert.Equal(t, "Нов отговор", got.answer)
	assert.True(t, got.regenerated)
	assert.True(t, got.heldLock)
}

func TestAnAnswerOfTheTelegramBotItselfIsNotRepeatedBackToIt(t *testing.T) {
	f, rec := newMirrorFixture(t, "Отговор")

	_, err := f.orchestrator.ProcessChatTurn(context.Background(), f.userID, ChatRequest{SessionID: "tg-42", Content: "От Telegram"})

	require.NoError(t, err)
	assert.Empty(t, rec.all())
}

func TestWithoutAMirrorNothingHappens(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})
	w := newLiveWriter()

	f.orchestrator.HandleStreamChat(w, req)

	assert.Contains(t, w.body(), "data: [DONE]")
	assert.Equal(t, []string{"user: Здравей", "assistant: Отговор"}, f.stored(t, "tg-42"))
}

func TestAMirrorThatIsTakenAwayStopsRepeating(t *testing.T) {
	f, rec := newMirrorFixture(t, "Отговор")
	f.orchestrator.SetReplyMirror(nil)
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})

	f.orchestrator.HandleStreamChat(newLiveWriter(), req)

	assert.Empty(t, rec.all())
}

// The answer is given and stored whatever happens in the repeat.
func TestAMirrorThatPanicsDoesNotLoseTheAnswer(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	f.orchestrator.SetReplyMirror(func(context.Context, uuid.UUID, string, string, string, bool) {
		panic("telegram blew up")
	})
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})
	w := newLiveWriter()

	require.NotPanics(t, func() { f.orchestrator.HandleStreamChat(w, req) })

	assert.Contains(t, w.body(), "data: [DONE]")
	assert.Equal(t, []string{"user: Здравей", "assistant: Отговор"}, f.stored(t, "tg-42"))
	assert.Zero(t, f.orchestrator.sessions.size(), "the conversation was let go")
}

// A slow Telegram does not hold the conversation longer than mirrorTimeout: the context of the
// repeat ends then (and does not end with the request of a client that has left).
func TestTheRepeatHasItsOwnShortTime(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	var remaining time.Duration
	var alive bool
	f.orchestrator.SetReplyMirror(func(ctx context.Context, _ uuid.UUID, _, _, _ string, _ bool) {
		deadline, ok := ctx.Deadline()
		if ok {
			remaining = time.Until(deadline)
		}
		alive = ctx.Err() == nil
	})
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-42", Content: "Здравей"})

	f.orchestrator.HandleStreamChat(newLiveWriter(), req)

	assert.True(t, alive)
	assert.Greater(t, remaining, 5*time.Second)
	assert.LessOrEqual(t, remaining, 10*time.Second, "the bound is 10 seconds")
}
