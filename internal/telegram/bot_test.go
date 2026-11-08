package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/agent"
	"go-monolith/internal/proposals"
)

type fakeSender struct {
	mu       sync.Mutex
	messages []tgbotapi.MessageConfig
	// events is the order in which things reached the chat: "typing" (recorded
	// when the request has finished) and "message" (recorded when it starts).
	events []string

	// typingDelay and messageDelay make a request take that long, like a slow
	// network. Set them before the bot starts.
	typingDelay  time.Duration
	messageDelay time.Duration
}

func (s *fakeSender) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	switch m := c.(type) {
	case tgbotapi.ChatActionConfig:
		time.Sleep(s.typingDelay)
		s.mu.Lock()
		s.events = append(s.events, "typing")
		s.mu.Unlock()
	case tgbotapi.MessageConfig:
		s.mu.Lock()
		s.messages = append(s.messages, m)
		s.events = append(s.events, "message")
		s.mu.Unlock()
		time.Sleep(s.messageDelay)
	}
	return tgbotapi.Message{}, nil
}

func (s *fakeSender) eventLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *fakeSender) typingCount() int {
	n := 0
	for _, e := range s.eventLog() {
		if e == "typing" {
			n++
		}
	}
	return n
}

func (s *fakeSender) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.messages))
	for _, m := range s.messages {
		out = append(out, m.Text)
	}
	return out
}

type fakeStore struct {
	mu         sync.Mutex
	linked     map[int64]uuid.UUID
	lookupErr  error
	upsertErr  error
	linkErr    error
	upserts    []uuid.UUID
	linkCalls  int
	lookupHits int
}

func newFakeStore() *fakeStore { return &fakeStore{linked: map[int64]uuid.UUID{}} }

func (s *fakeStore) LinkedUser(_ context.Context, chatID int64) (uuid.UUID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupHits++
	if s.lookupErr != nil {
		return uuid.Nil, false, s.lookupErr
	}
	id, ok := s.linked[chatID]
	return id, ok, nil
}

func (s *fakeStore) UpsertUser(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, id)
	return s.upsertErr
}

func (s *fakeStore) LinkChat(_ context.Context, chatID int64, userID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.linkCalls++
	if s.linkErr != nil {
		return s.linkErr
	}
	s.linked[chatID] = userID
	return nil
}

type fakeProcessor struct {
	calls  atomic.Int32
	answer string
	err    error
	// proposals are the writes the model "proposed" in the turn.
	proposals []proposals.Proposal
	// onCall runs inside ProcessChatTurn and may block.
	onCall func(ctx context.Context, userID uuid.UUID, req agent.ChatRequest)

	mu    sync.Mutex
	users []uuid.UUID
	reqs  []agent.ChatRequest
}

func (p *fakeProcessor) ProcessChatTurn(ctx context.Context, userID uuid.UUID, req agent.ChatRequest) (agent.ChatTurn, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.users = append(p.users, userID)
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	if p.onCall != nil {
		p.onCall(ctx, userID, req)
	}
	return agent.ChatTurn{Answer: p.answer, Proposals: p.proposals}, p.err
}

// fakeDesk is the proposal desk: it holds the open proposals per user.
type fakeDesk struct {
	mu       sync.Mutex
	open     map[uuid.UUID][]proposals.Proposal
	decided  []string
	failWith error
}

func newFakeDesk() *fakeDesk { return &fakeDesk{open: map[uuid.UUID][]proposals.Proposal{}} }

func (d *fakeDesk) Pending(_ context.Context, userID uuid.UUID) ([]proposals.Proposal, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]proposals.Proposal(nil), d.open[userID]...), d.failWith
}

func (d *fakeDesk) decide(userID uuid.UUID, id int64, verb string) (proposals.Proposal, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failWith != nil {
		return proposals.Proposal{}, d.failWith
	}
	for i, p := range d.open[userID] {
		if p.ID == id {
			d.open[userID] = append(d.open[userID][:i:i], d.open[userID][i+1:]...)
			d.decided = append(d.decided, fmt.Sprintf("%s %d", verb, id))
			return p, nil
		}
	}
	return proposals.Proposal{}, proposals.ErrNotFound
}

func (d *fakeDesk) Approve(_ context.Context, userID uuid.UUID, id int64) (proposals.Proposal, error) {
	return d.decide(userID, id, "approve")
}

func (d *fakeDesk) Reject(_ context.Context, userID uuid.UUID, id int64) (proposals.Proposal, error) {
	return d.decide(userID, id, "reject")
}

type fixture struct {
	bot       *Bot
	sender    *fakeSender
	store     *fakeStore
	processor *fakeProcessor
	desk      *fakeDesk
}

func newFixture(allowed ...int64) *fixture {
	f := &fixture{sender: &fakeSender{}, store: newFakeStore(), processor: &fakeProcessor{answer: "отговор"}, desk: newFakeDesk()}
	f.bot = newBot(f.sender, f.processor, f.store, f.desk, allowed)
	return f
}

func privateMessage(chatID int64, text string) *tgbotapi.Message {
	return &tgbotapi.Message{Text: text, Chat: &tgbotapi.Chat{ID: chatID, Type: "private"}}
}

func TestUnauthorizedChatGetsNothingAndTouchesNothing(t *testing.T) {
	tests := []struct {
		name    string
		allowed []int64
		text    string
	}{
		{name: "plain message", allowed: []int64{1}, text: "здравей"},
		{name: "link attempt", allowed: []int64{1}, text: "/link " + uuid.NewString()},
		{name: "empty allowlist denies everyone", allowed: nil, text: "здравей"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(tc.allowed...)

			f.bot.handleMessage(context.Background(), privateMessage(999, tc.text))

			assert.Empty(t, f.sender.texts(), "an unauthorised chat must get no reply")
			assert.Zero(t, f.processor.calls.Load(), "the LLM must not run")
			assert.Empty(t, f.store.upserts, "no users row may be created")
			assert.Zero(t, f.store.lookupHits)
			assert.Zero(t, f.store.linkCalls)
		})
	}
}

func TestIDCommandAnswersEveryone(t *testing.T) {
	f := newFixture(1)

	f.bot.handleMessage(context.Background(), privateMessage(999, "/id"))
	f.bot.handleMessage(context.Background(), privateMessage(999, "/ID@Lili_Smart_bot"))

	assert.Equal(t, []string{"Chat id: 999", "Chat id: 999"}, f.sender.texts())
	assert.Zero(t, f.processor.calls.Load())
	assert.Empty(t, f.store.upserts)
}

func TestAllowedChatIsAnswered(t *testing.T) {
	f := newFixture(42)

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	assert.Equal(t, []string{"отговор"}, f.sender.texts())
	require.Equal(t, int32(1), f.processor.calls.Load())
	assert.Equal(t, "tg-42", f.processor.reqs[0].SessionID)
	assert.Equal(t, "Здравей", f.processor.reqs[0].Content)
	assert.Equal(t, f.bot.generateDeterministicUUID(42), f.processor.users[0], "an unlinked chat uses its derived user")
	assert.Equal(t, []uuid.UUID{f.bot.generateDeterministicUUID(42)}, f.store.upserts)
}

func TestLinkedChatUsesLinkedUser(t *testing.T) {
	f := newFixture(42)
	linkedTo := uuid.New()
	f.store.linked[42] = linkedTo

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Equal(t, int32(1), f.processor.calls.Load())
	assert.Equal(t, linkedTo, f.processor.users[0])
	assert.Empty(t, f.store.upserts)
}

func TestUserLookupFailureDoesNotFallBackToAnotherUser(t *testing.T) {
	f := newFixture(42)
	f.store.lookupErr = errors.New("connection refused")

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	assert.Zero(t, f.processor.calls.Load())
	assert.Empty(t, f.store.upserts)
	require.Len(t, f.sender.texts(), 1)
	assert.Contains(t, f.sender.texts()[0], "грешка")
}

func TestLink(t *testing.T) {
	target := uuid.New()

	t.Run("links an allowed chat", func(t *testing.T) {
		f := newFixture(42)

		f.bot.handleMessage(context.Background(), privateMessage(42, "/link "+target.String()))

		assert.Equal(t, target, f.store.linked[42])
		assert.Equal(t, []uuid.UUID{target}, f.store.upserts)
		require.Len(t, f.sender.texts(), 1)
		assert.Contains(t, f.sender.texts()[0], "Успешно свързан")
		assert.Zero(t, f.processor.calls.Load(), "/link is not a question for the model")
	})

	t.Run("accepts the @botname form", func(t *testing.T) {
		f := newFixture(42)

		f.bot.handleMessage(context.Background(), privateMessage(42, "/link@Lili_Smart_bot "+target.String()))

		assert.Equal(t, target, f.store.linked[42])
	})

	t.Run("rejects an invalid uuid", func(t *testing.T) {
		f := newFixture(42)

		f.bot.handleMessage(context.Background(), privateMessage(42, "/link not-a-uuid"))

		assert.Equal(t, []string{"Невалиден UUID."}, f.sender.texts())
		assert.Zero(t, f.store.linkCalls)
		assert.Empty(t, f.store.upserts)
	})

	t.Run("does not relink an already linked chat", func(t *testing.T) {
		f := newFixture(42)
		existing := uuid.New()
		f.store.linked[42] = existing

		f.bot.handleMessage(context.Background(), privateMessage(42, "/link "+target.String()))

		assert.Equal(t, []string{"Вече си свързан."}, f.sender.texts())
		assert.Equal(t, existing, f.store.linked[42])
		assert.Zero(t, f.store.linkCalls)
	})

	t.Run("a database error is not read as not linked", func(t *testing.T) {
		f := newFixture(42)
		f.store.lookupErr = errors.New("connection refused")

		f.bot.handleMessage(context.Background(), privateMessage(42, "/link "+target.String()))

		assert.Zero(t, f.store.linkCalls, "must not relink on a failed lookup")
		assert.Empty(t, f.store.upserts)
		assert.Empty(t, f.store.linked)
	})
}

func TestTooLongMessageIsRejectedBeforeAnyWork(t *testing.T) {
	f := newFixture(42)

	f.bot.handleMessage(context.Background(), privateMessage(42, strings.Repeat("а", 50000)))

	assert.Zero(t, f.processor.calls.Load())
	assert.Empty(t, f.store.upserts)
	require.Len(t, f.sender.texts(), 1)
	assert.Contains(t, f.sender.texts()[0], "твърде дълго")
}

func TestEmptyAnswerGetsAFallbackReply(t *testing.T) {
	f := newFixture(42)
	f.processor.answer = "  \n "

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Len(t, f.sender.texts(), 1)
	assert.Contains(t, f.sender.texts()[0], "Не успях")
}

func TestProcessorErrorIsReportedGenerically(t *testing.T) {
	f := newFixture(42)
	f.processor.err = errors.New(`Post "http://10.1.2.3:13305/v1": refused`)

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Len(t, f.sender.texts(), 1)
	assert.NotContains(t, f.sender.texts()[0], "10.1.2.3")
}

func TestGroupAndMediaMessages(t *testing.T) {
	f := newFixture(7, 42)

	group := &tgbotapi.Message{Text: "здравей", Chat: &tgbotapi.Chat{ID: 7, Type: "group"}}
	f.bot.handleMessage(context.Background(), group)
	voice := privateMessage(42, "")
	voice.Voice = &tgbotapi.Voice{}
	f.bot.handleMessage(context.Background(), voice)

	assert.Zero(t, f.processor.calls.Load())
	require.Len(t, f.sender.texts(), 2)
	assert.Contains(t, f.sender.texts()[0], "лични чатове")
	assert.Contains(t, f.sender.texts()[1], "само текст")
}

// Two messages from one chat must not run side by side, otherwise their
// replies can arrive swapped.
func TestTurnsOfOneChatAreSerialised(t *testing.T) {
	f := newFixture(42)
	var running, maxRunning atomic.Int32
	f.processor.onCall = func(context.Context, uuid.UUID, agent.ChatRequest) {
		n := running.Add(1)
		for {
			prev := maxRunning.Load()
			if n <= prev || maxRunning.CompareAndSwap(prev, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		running.Add(-1)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.bot.handleMessage(context.Background(), privateMessage(42, "въпрос"))
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(5), f.processor.calls.Load())
	assert.Equal(t, int32(1), maxRunning.Load(), "turns of one chat must run one at a time")
}

// Different chats run in parallel, but never more than the global limit.
func TestTurnsAreCappedGlobally(t *testing.T) {
	chats := []int64{1, 2, 3, 4, 5, 6}
	f := newFixture(chats...)
	var running, maxRunning atomic.Int32
	f.processor.onCall = func(context.Context, uuid.UUID, agent.ChatRequest) {
		n := running.Add(1)
		for {
			prev := maxRunning.Load()
			if n <= prev || maxRunning.CompareAndSwap(prev, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		running.Add(-1)
	}

	var wg sync.WaitGroup
	for _, chat := range chats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.bot.handleMessage(context.Background(), privateMessage(chat, "въпрос"))
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(len(chats)), f.processor.calls.Load())
	assert.Equal(t, int32(maxConcurrentChats), maxRunning.Load(), "the cap must be reached but not exceeded")
}

func TestCancellationReleasesWaitingTurns(t *testing.T) {
	f := newFixture(42)
	started := make(chan struct{})
	f.processor.onCall = func(ctx context.Context, _ uuid.UUID, _ agent.ChatRequest) {
		close(started)
		<-ctx.Done()
	}
	f.processor.err = context.Canceled

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { f.bot.handleMessage(ctx, privateMessage(42, "първи")); done <- struct{}{} }()
	<-started
	go func() { f.bot.handleMessage(ctx, privateMessage(42, "втори")); done <- struct{}{} }()
	time.Sleep(20 * time.Millisecond) // let the second one queue behind the chat lock

	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a turn is still blocked after cancellation")
		}
	}
	assert.Empty(t, f.sender.texts(), "no error reply while shutting down")
	assert.Equal(t, int32(1), f.processor.calls.Load(), "the queued turn must not start after cancellation")
}

func TestSendReplySplitsLongTextWithoutBreakingRunes(t *testing.T) {
	f := newFixture(42)
	word := "дума "
	text := strings.Repeat(word, 2000) // 10 000 runes of Cyrillic

	f.bot.sendReply(42, text)

	parts := f.sender.texts()
	require.Greater(t, len(parts), 1)
	var joined []string
	for _, p := range parts {
		assert.LessOrEqual(t, utf8.RuneCountInString(p), maxReplyRunes)
		assert.True(t, utf8.ValidString(p))
		joined = append(joined, p)
	}
	assert.Equal(t, strings.Fields(text), strings.Fields(strings.Join(joined, " ")), "no words may be lost or cut")
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in      string
		command string
		args    string
	}{
		{"/link abc", "link", "abc"},
		{"/LINK@Bot  abc ", "link", "abc"},
		{"/id", "id", ""},
		{"/link\nabc", "link", "abc"},
		{"просто текст", "", "просто текст"},
		{"", "", ""},
	}
	for _, tc := range tests {
		command, args := parseCommand(tc.in)
		assert.Equal(t, tc.command, command, tc.in)
		assert.Equal(t, tc.args, args, tc.in)
	}
}

// A turn runs in a goroutine of its own, outside the HTTP recovery middleware
// and River. Without recoverTurn a panic in it (a bug in the model client, a
// nil map in a tool) ends the whole backend: this test would crash the test
// binary instead of failing.
func TestAPanicInOneTurnDoesNotTakeTheProcessDown(t *testing.T) {
	f := newFixture(42)
	f.processor.onCall = func(context.Context, uuid.UUID, agent.ChatRequest) {
		panic("model client blew up")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer f.bot.recoverTurn(42)
		f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))
	}()
	<-done

	require.Len(t, f.sender.texts(), 1, "the user is told something went wrong instead of getting silence")
	assert.Contains(t, f.sender.texts()[0], "грешка")

	// The bot keeps serving the same chat afterwards.
	f.processor.onCall = nil
	f.bot.handleMessage(context.Background(), privateMessage(42, "Пак здравей"))
	assert.Equal(t, "отговор", f.sender.texts()[len(f.sender.texts())-1])
}

func TestRecoverTurnDoesNothingWithoutAPanic(t *testing.T) {
	f := newFixture(42)

	func() { defer f.bot.recoverTurn(42) }()

	assert.Empty(t, f.sender.texts())
}
