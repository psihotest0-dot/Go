package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
)

// queuedBehind is how many turns wait for the conversation, one of them holding it (for tests).
func (s *sessionLocks) queuedBehind(userID uuid.UUID, sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.m[userID.String()+"|"+sessionID]
	if l == nil {
		return 0
	}
	return l.users - 1
}

// within runs fn and fails the test when it does not return in time: a lock that is never freed
// would otherwise hang the whole run. fn must not use require (it runs in another goroutine).
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s", what, d)
	}
}

// --- the lock on its own -------------------------------------------------------------------

func TestSessionLockLetsOnlyOneTurnIn(t *testing.T) {
	var s sessionLocks
	user := uuid.New()
	var inside atomic.Int32
	var overlapped atomic.Bool

	var wg sync.WaitGroup
	for g := 0; g < 40; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 25; k++ {
				unlock, err := s.lock(context.Background(), user, "s", nil)
				if err != nil {
					overlapped.Store(true)
					return
				}
				if inside.Add(1) != 1 {
					overlapped.Store(true)
				}
				runtime.Gosched()
				inside.Add(-1)
				unlock()
			}
		}()
	}
	within(t, 20*time.Second, "forty turns taking turns", wg.Wait)

	assert.False(t, overlapped.Load(), "two turns were inside one conversation at once")
	assert.Zero(t, s.size(), "no entry is left when nobody holds or waits")
}

func TestSessionLockKeepsConversationsApart(t *testing.T) {
	var s sessionLocks
	alice, bob := uuid.New(), uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	hold, err := s.lock(ctx, alice, "chat", nil)
	require.NoError(t, err)
	defer hold()

	// Another session of the same person, the same session name of another person: both are free.
	otherSession, err := s.lock(ctx, alice, "chat-2", func() { t.Error("another session must not wait") })
	require.NoError(t, err)
	otherSession()
	otherPerson, err := s.lock(ctx, bob, "chat", func() { t.Error("another person must not wait") })
	require.NoError(t, err)
	otherPerson()
}

func TestSessionLockCallsWaitingOnlyWhenTheConversationIsTaken(t *testing.T) {
	var s sessionLocks
	user := uuid.New()
	var told atomic.Int32

	free, err := s.lock(context.Background(), user, "s", func() { told.Add(1) })
	require.NoError(t, err)
	assert.Zero(t, told.Load(), "a free conversation is not announced as busy")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = s.lock(ctx, user, "s", func() { told.Add(1) })
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int32(1), told.Load(), "told once, when the wait began")
	free()
}

func TestSessionLockGivingUpLeavesNothingBehind(t *testing.T) {
	var s sessionLocks
	user := uuid.New()
	hold, err := s.lock(context.Background(), user, "s", nil)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_, err := s.lock(ctx, user, "s", nil)
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	assert.Equal(t, 0, s.queuedBehind(user, "s"), "the turns that gave up are not waiting any more")

	hold()
	assert.Zero(t, s.size(), "the entry goes when the holder leaves")
	within(t, 2*time.Second, "taking the free conversation", func() {
		unlock, err := s.lock(context.Background(), user, "s", nil)
		if err == nil {
			unlock()
		}
	})
}

func TestSessionLockUnlockTwiceFreesItOnce(t *testing.T) {
	var s sessionLocks
	user := uuid.New()

	first, err := s.lock(context.Background(), user, "s", nil)
	require.NoError(t, err)
	// A second unlock must not take a place away from the next holder or block.
	within(t, 2*time.Second, "a second unlock", func() { first(); first() })

	next, err := s.lock(context.Background(), user, "s", nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.lock(ctx, user, "s", nil)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the conversation is held by the next turn")
	next()
	assert.Zero(t, s.size())
}

func TestSessionLockIsFreedWhenATurnPanics(t *testing.T) {
	var s sessionLocks
	user := uuid.New()

	func() {
		defer func() { _ = recover() }()
		unlock, err := s.lock(context.Background(), user, "s", nil)
		require.NoError(t, err)
		defer unlock()
		panic("the turn failed")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := s.lock(ctx, user, "s", func() { t.Error("the lock was not freed") })
	require.NoError(t, err)
	unlock()
}

func TestSessionLockServesTheWaitersInTheOrderTheyCame(t *testing.T) {
	var s sessionLocks
	user := uuid.New()
	hold, err := s.lock(context.Background(), user, "s", nil)
	require.NoError(t, err)

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 1; i <= 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := s.lock(context.Background(), user, "s", nil)
			if err != nil {
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			unlock()
		}()
		require.Eventually(t, func() bool { return s.queuedBehind(user, "s") == i }, 2*time.Second, time.Millisecond)
		time.Sleep(20 * time.Millisecond) // the goroutine is parked on the lock before the next one starts
	}
	hold()
	within(t, 5*time.Second, "the waiters", wg.Wait)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{1, 2, 3, 4}, order)
}

// --- through the handlers ------------------------------------------------------------------

// gatedLLM holds its first ChatStream call until open is called and says when that call began: a
// generation that takes a long time.
type gatedLLM struct {
	llm.LLMClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (g *gatedLLM) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (llm.Stream, error) {
	if g.calls.Add(1) == 1 {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.LLMClient.ChatStream(ctx, req)
}

func (g *gatedLLM) open() { g.once.Do(func() { close(g.release) }) }

// mock is the model behind the gate.
func (g *gatedLLM) mock() *llm.MockLLM { return g.LLMClient.(*llm.MockLLM) }

// gateFirstCall makes the fixture's model hold the first generation.
func (f *sanitizeFixture) gateFirstCall(t *testing.T) *gatedLLM {
	t.Helper()
	g := &gatedLLM{LLMClient: f.orchestrator.llmClient, entered: make(chan struct{}), release: make(chan struct{})}
	f.orchestrator.llmClient = g
	t.Cleanup(g.open)
	return g
}

func (g *gatedLLM) waitUntilGenerating(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first turn never reached the model")
	}
}

// liveWriter is a response writer that can be read while a handler still writes to it.
type liveWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	code int
	head http.Header
}

func newLiveWriter() *liveWriter { return &liveWriter{head: http.Header{}} }

func (w *liveWriter) Header() http.Header { return w.head }
func (w *liveWriter) Flush()              {}
func (w *liveWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = code
	}
}
func (w *liveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.buf.Write(p)
}
func (w *liveWriter) body() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
func (w *liveWriter) status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.code
}

func requestAs(userID uuid.UUID, path string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	return req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, userID))
}

// run starts a handler and returns the channel that closes when it has returned.
func run(handler func(http.ResponseWriter, *http.Request), w http.ResponseWriter, req *http.Request) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); handler(w, req) }()
	return done
}

func waitFor(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}

// stored returns the conversation as "role: content", oldest first.
func (f *sanitizeFixture) stored(t *testing.T, sessionID string) []string {
	t.Helper()
	msgs, err := f.database.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{
		UserID: f.userID, SessionID: sessionID, Limit: 50,
	})
	require.NoError(t, err)
	out := make([]string, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		out = append(out, msgs[i].Role+": "+msgs[i].Content)
	}
	return out
}

// answersInOrder makes the model give these answers to its first calls, one for each.
func (f *sanitizeFixture) answersInOrder(answers ...string) {
	mock := f.local()
	mock.StreamScript = nil
	for _, a := range answers {
		mock.StreamScript = append(mock.StreamScript, say(a).StreamScript[0])
	}
}

// holdFor fails when, during d, the condition ever breaks: "nothing happens while it waits".
func holdFor(t *testing.T, d time.Duration, still func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		require.True(t, still(), what)
		time.Sleep(10 * time.Millisecond)
	}
}

// The reason for the lock: a message from the app and one from Telegram in one conversation are
// answered one after the other, and the second reads the first answer in its history.
func TestTwoTurnsOfOneConversationTakeTurns(t *testing.T) {
	f := newSanitizeFixture(t, "x")
	f.answersInOrder("Първи отговор", "Втори отговор")
	gate := f.gateFirstCall(t)

	w1, w2 := newLiveWriter(), newLiveWriter()
	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-lock", Content: "Първи въпрос"})
	_, req2 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-lock", Content: "Втори въпрос"})
	done1 := run(f.orchestrator.HandleStreamChat, w1, req1)
	gate.waitUntilGenerating(t)
	done2 := run(f.orchestrator.HandleStreamChat, w2, req2)
	require.Eventually(t, func() bool { return f.orchestrator.sessions.queuedBehind(f.userID, "s-lock") == 1 }, 5*time.Second, 5*time.Millisecond)

	holdFor(t, 200*time.Millisecond, func() bool {
		return gate.calls.Load() == 1 && len(f.stored(t, "s-lock")) == 1
	}, "the second turn stored its question or asked the model while the first one was still being answered")

	gate.open()
	waitFor(t, done1, "the first turn")
	waitFor(t, done2, "the second turn")

	assert.Equal(t, []string{
		"user: Първи въпрос", "assistant: Първи отговор",
		"user: Втори въпрос", "assistant: Втори отговор",
	}, f.stored(t, "s-lock"))
	assert.Contains(t, w2.body(), "Втори отговор")

	reqs := gate.mock().Requests
	require.Len(t, reqs, 2)
	var second []string
	for _, m := range reqs[1].Messages {
		second = append(second, m.Content)
	}
	assert.Contains(t, strings.Join(second, "\n"), "Първи отговор", "the second turn read the first answer in its history")
	assert.Zero(t, f.orchestrator.sessions.size())
}

func TestOtherConversationsAreNotHeldUp(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	gate := f.gateFirstCall(t)
	other, err := f.database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, f.database.Pool, other.ID) })

	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-held", Content: "Бавен въпрос"})
	done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
	gate.waitUntilGenerating(t)

	// Another session of the same person, and the same session name of another person.
	_, sameUser := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-free", Content: "Друг разговор"})
	w3 := newLiveWriter()
	otherPerson := requestAs(other.ID, "/chat/stream", ChatRequest{SessionID: "s-held", Content: "Чужд разговор"})
	w4 := newLiveWriter()
	done3 := run(f.orchestrator.HandleStreamChat, w3, sameUser)
	done4 := run(f.orchestrator.HandleStreamChat, w4, otherPerson)
	waitFor(t, done3, "another session of the same person")
	waitFor(t, done4, "the same session name of another person")

	assert.Contains(t, w3.body(), "data: [DONE]")
	assert.Contains(t, w4.body(), "data: [DONE]")
	select {
	case <-done1:
		t.Fatal("the held turn finished before it was let go")
	default:
	}
	gate.open()
	waitFor(t, done1, "the held turn")
	assert.Zero(t, f.orchestrator.sessions.size())
}

// Only a client that asks (queue_events) is told it waits, and the frame comes before anything
// else: the old clients take a frame they do not know for text.
func TestAWaitingTurnIsToldSoWhenItAskedFor(t *testing.T) {
	f := newSanitizeFixture(t, "x")
	f.answersInOrder("Първи отговор", "Втори отговор")
	gate := f.gateFirstCall(t)

	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-queued", Content: "Първи въпрос"})
	done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
	gate.waitUntilGenerating(t)

	w := newLiveWriter()
	_, req2 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-queued", Content: "Втори въпрос", QueueEvents: true})
	done2 := run(f.orchestrator.HandleStreamChat, w, req2)

	require.Eventually(t, func() bool { return strings.Contains(w.body(), "event: queued\ndata: {}\n\n") }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, http.StatusOK, w.status())
	assert.Equal(t, "text/event-stream", w.head.Get("Content-Type"))
	assert.NotContains(t, w.body(), "event: sources", "the answer has not begun")

	gate.open()
	waitFor(t, done1, "the first turn")
	waitFor(t, done2, "the second turn")
	body := w.body()
	assert.Less(t, strings.Index(body, "event: queued"), strings.Index(body, "event: sources"), "the waiting frame comes first")
	assert.Equal(t, 1, strings.Count(body, "event: queued"))
	assert.Contains(t, body, "Втори отговор")
	assert.Contains(t, body, "data: [DONE]")
}

func TestATurnThatDidNotAskIsNotToldAndGetsTheUsualStream(t *testing.T) {
	f := newSanitizeFixture(t, "x")
	f.answersInOrder("Първи отговор", "Втори отговор")
	gate := f.gateFirstCall(t)

	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-quiet", Content: "Първи въпрос"})
	done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
	gate.waitUntilGenerating(t)

	w := newLiveWriter()
	_, req2 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-quiet", Content: "Втори въпрос"})
	done2 := run(f.orchestrator.HandleStreamChat, w, req2)
	require.Eventually(t, func() bool { return f.orchestrator.sessions.queuedBehind(f.userID, "s-quiet") == 1 }, 5*time.Second, 5*time.Millisecond)
	holdFor(t, 150*time.Millisecond, func() bool { return w.body() == "" }, "a client that did not ask was sent something while it waited")

	gate.open()
	waitFor(t, done1, "the first turn")
	waitFor(t, done2, "the second turn")
	assert.NotContains(t, w.body(), "queued")
	assert.True(t, strings.HasPrefix(w.body(), "event: sources"), "the stream begins as it always did: %q", w.body())
}

func TestAFreeConversationSendsNoWaitingFrame(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	w := newLiveWriter()
	_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-free-q", Content: "Въпрос", QueueEvents: true})

	f.orchestrator.HandleStreamChat(w, req)

	assert.NotContains(t, w.body(), "queued")
	assert.Contains(t, w.body(), "data: [DONE]")
}

// A turn that waits until its time is out stores nothing: not its question either, otherwise the
// next answer would be given to a question the person already knows was refused.
func TestATurnThatGivesUpWaitingStoresNothingAndSaysWhy(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	gate := f.gateFirstCall(t)
	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-busy", Content: "Бавен въпрос"})
	done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
	gate.waitUntilGenerating(t)

	t.Run("without the waiting frame it is a plain HTTP refusal", func(t *testing.T) {
		w := newLiveWriter()
		_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-busy", Content: "Нетърпелив въпрос"})
		ctx, cancel := context.WithTimeout(req.Context(), 50*time.Millisecond)
		defer cancel()
		waitFor(t, run(f.orchestrator.HandleStreamChat, w, req.WithContext(ctx)), "the turn that gave up")

		assert.Equal(t, http.StatusServiceUnavailable, w.status())
		assert.Contains(t, w.body(), busyMessage)
	})
	t.Run("after the waiting frame it is an error frame", func(t *testing.T) {
		w := newLiveWriter()
		_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-busy", Content: "Нетърпелив въпрос", QueueEvents: true})
		ctx, cancel := context.WithTimeout(req.Context(), 50*time.Millisecond)
		defer cancel()
		waitFor(t, run(f.orchestrator.HandleStreamChat, w, req.WithContext(ctx)), "the turn that gave up")

		assert.Equal(t, http.StatusOK, w.status(), "the status was sent with the waiting frame")
		assert.Contains(t, w.body(), "event: queued")
		assert.Contains(t, w.body(), "event: error\ndata: {\"error\":\""+busyMessage)
		assert.NotContains(t, w.body(), "[DONE]")
	})
	t.Run("a client that left is sent nothing", func(t *testing.T) {
		w := newLiveWriter()
		_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-busy", Content: "Отказан въпрос"})
		ctx, cancel := context.WithCancel(req.Context())
		done := run(f.orchestrator.HandleStreamChat, w, req.WithContext(ctx))
		require.Eventually(t, func() bool { return f.orchestrator.sessions.queuedBehind(f.userID, "s-busy") == 1 }, 5*time.Second, 5*time.Millisecond)
		cancel()
		waitFor(t, done, "the turn of the client that left")

		assert.Empty(t, w.body())
	})

	assert.Equal(t, []string{"user: Бавен въпрос"}, f.stored(t, "s-busy"), "only the turn that was served stored its question")
	gate.open()
	waitFor(t, done1, "the held turn")
	assert.Equal(t, []string{"user: Бавен въпрос", "assistant: Отговор"}, f.stored(t, "s-busy"))
	assert.Zero(t, f.orchestrator.sessions.size())
}

// A regeneration picks the answers it replaces out of the history. Without the lock it reads
// the history while a turn is still being answered, and its replacement ends beside that answer.
func TestARegenerationWaitsForTheTurnThatIsBeingAnswered(t *testing.T) {
	f := newSanitizeFixture(t, "x")
	f.answersInOrder("Първи отговор", "Нов отговор")
	f.seedTurn(t, "s-regen-lock", "Стар въпрос", "Стар отговор")
	gate := f.gateFirstCall(t)

	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-regen-lock", Content: "Нов въпрос"})
	done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
	gate.waitUntilGenerating(t)

	_, req2 := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-regen-lock"})
	done2 := run(f.orchestrator.HandleRegenerate, newLiveWriter(), req2)
	require.Eventually(t, func() bool { return f.orchestrator.sessions.queuedBehind(f.userID, "s-regen-lock") == 1 }, 5*time.Second, 5*time.Millisecond)
	holdFor(t, 150*time.Millisecond, func() bool { return gate.calls.Load() == 1 }, "the regeneration asked the model while the turn was still being answered")

	gate.open()
	waitFor(t, done1, "the turn")
	waitFor(t, done2, "the regeneration")

	assert.Equal(t, []string{
		"user: Стар въпрос", "assistant: Стар отговор",
		"user: Нов въпрос", "assistant: Нов отговор",
	}, f.stored(t, "s-regen-lock"), "the regeneration replaced the answer of the new question, once")
	assert.Zero(t, f.orchestrator.sessions.size())
}

func TestARegenerationThatWaitedCanAlsoBeToldSo(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	f.seedTurn(t, "s-regen-q", "Въпрос", "Стар отговор")
	gate := f.gateFirstCall(t)
	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-regen-q", Content: "Бавен въпрос"})
	done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
	gate.waitUntilGenerating(t)

	w := newLiveWriter()
	_, req2 := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-regen-q", QueueEvents: true})
	done2 := run(f.orchestrator.HandleRegenerate, w, req2)
	require.Eventually(t, func() bool { return strings.Contains(w.body(), "event: queued") }, 5*time.Second, 5*time.Millisecond)

	gate.open()
	waitFor(t, done1, "the turn")
	waitFor(t, done2, "the regeneration")
	assert.Contains(t, w.body(), "data: [DONE]")
}

// Telegram goes through ProcessChatTurn and has to wait for the app's turn too; when its time
// is out first it gets the error it can say something about, and nothing of its message is kept.
func TestATelegramTurnWaitsForTheAppAndGivesUpWithAnErrorItCanRecognise(t *testing.T) {
	f := newSanitizeFixture(t, "x")
	f.answersInOrder("Отговор в приложението", "Отговор в Telegram")
	gate := f.gateFirstCall(t)

	_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "tg-77", Content: "От приложението"})
	done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
	gate.waitUntilGenerating(t)

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.orchestrator.ProcessChatTurn(short, f.userID, ChatRequest{SessionID: "tg-77", Content: "Прекалено нетърпеливо"})
	require.ErrorIs(t, err, ErrConversationBusy)
	assert.Equal(t, []string{"user: От приложението"}, f.stored(t, "tg-77"))

	type result struct {
		turn ChatTurn
		err  error
	}
	res := make(chan result, 1)
	go func() {
		turn, err := f.orchestrator.ProcessChatTurn(context.Background(), f.userID, ChatRequest{SessionID: "tg-77", Content: "От Telegram"})
		res <- result{turn, err}
	}()
	require.Eventually(t, func() bool { return f.orchestrator.sessions.queuedBehind(f.userID, "tg-77") == 1 }, 5*time.Second, 5*time.Millisecond)

	gate.open()
	waitFor(t, done1, "the app's turn")
	select {
	case r := <-res:
		require.NoError(t, r.err)
		assert.Equal(t, "Отговор в Telegram", r.turn.Answer)
	case <-time.After(10 * time.Second):
		t.Fatal("the Telegram turn did not finish")
	}
	assert.Equal(t, []string{
		"user: От приложението", "assistant: Отговор в приложението",
		"user: От Telegram", "assistant: Отговор в Telegram",
	}, f.stored(t, "tg-77"))
	assert.Zero(t, f.orchestrator.sessions.size())
}

// Every way out of a turn frees the conversation: a model that fails, a question the database
// refuses, an error after the waiting frame.
func TestEveryWayOutOfATurnFreesTheConversation(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор")
	freeAfterwards := func(t *testing.T, session string) {
		t.Helper()
		assert.Zero(t, f.orchestrator.sessions.size())
		w := newLiveWriter()
		_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: session, Content: "Следващ въпрос"})
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		f.orchestrator.HandleStreamChat(w, req.WithContext(ctx))
		assert.Contains(t, w.body(), "data: [DONE]", "the next turn was served")
	}

	t.Run("the model fails", func(t *testing.T) {
		f.local().Error = errors.New("model down")
		w := newLiveWriter()
		_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-fail", Content: "Въпрос"})
		f.orchestrator.HandleStreamChat(w, req)
		f.local().Error = nil
		assert.Contains(t, w.body(), "event: error")
		freeAfterwards(t, "s-fail")
	})
	t.Run("the question cannot be stored", func(t *testing.T) {
		w := newLiveWriter()
		_, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-nul", Content: "Нов\x00ред"})
		f.orchestrator.HandleStreamChat(w, req)
		assert.Equal(t, http.StatusInternalServerError, w.status())
		freeAfterwards(t, "s-nul")
	})
	t.Run("a regeneration with nothing to regenerate", func(t *testing.T) {
		w := newLiveWriter()
		_, req := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-empty"})
		f.orchestrator.HandleRegenerate(w, req)
		assert.Equal(t, http.StatusBadRequest, w.status())
		freeAfterwards(t, "s-empty")
	})
	t.Run("an error after the waiting frame is a frame", func(t *testing.T) {
		gate := f.gateFirstCall(t)
		_, req1 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-late", Content: "Бавен въпрос"})
		done1 := run(f.orchestrator.HandleStreamChat, newLiveWriter(), req1)
		gate.waitUntilGenerating(t)

		w := newLiveWriter()
		_, req2 := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-late", Content: "Нов\x00ред", QueueEvents: true})
		done2 := run(f.orchestrator.HandleStreamChat, w, req2)
		require.Eventually(t, func() bool { return strings.Contains(w.body(), "event: queued") }, 5*time.Second, 5*time.Millisecond)
		gate.open()
		waitFor(t, done1, "the first turn")
		waitFor(t, done2, "the second turn")

		assert.Equal(t, http.StatusOK, w.status())
		assert.Contains(t, w.body(), "event: error\ndata: {\"error\":\"internal server error\"}")
		assert.NotContains(t, w.body(), "HTTP/", "no second status line")
		freeAfterwards(t, "s-late")
	})
}

// size is the number of conversations that are held or waited for (for tests).
func (s *sessionLocks) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
