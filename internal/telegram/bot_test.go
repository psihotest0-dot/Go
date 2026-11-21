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

	// reject, when set, can make Send fail for a message; every try, failed or not, is in
	// attempts, and only the ones that went through are in messages.
	reject   func(tgbotapi.MessageConfig) error
	attempts []tgbotapi.MessageConfig
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
		s.attempts = append(s.attempts, m)
		reject := s.reject
		s.mu.Unlock()
		if reject != nil {
			if err := reject(m); err != nil {
				return tgbotapi.Message{}, err
			}
		}
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

// The conversation is shared with the app: when a long answer there keeps it busy until the turn's
// time is out, Telegram says so and does not report a failure of the model.
func TestABusyConversationIsSaidSoAndNotReportedAsAFailure(t *testing.T) {
	f := newFixture(42)
	f.processor.err = fmt.Errorf("%w: context deadline exceeded", agent.ErrConversationBusy)

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Len(t, f.sender.texts(), 1)
	assert.Contains(t, f.sender.texts()[0], "Разговорът още пише отговор")
	assert.NotContains(t, f.sender.texts()[0], "Възникна грешка")
}

// The answer of the model is Markdown: it goes out as the HTML Telegram shows, and a text the
// model wrote with markup characters of its own cannot become markup.
func TestAnAnswerIsSentAsHTML(t *testing.T) {
	f := newFixture(42)
	f.processor.answer = "**Здравей** & <b>свят</b>\n- `код`"

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Len(t, f.sender.messages, 1)
	assert.Equal(t, tgbotapi.ModeHTML, f.sender.messages[0].ParseMode)
	assert.Equal(t, "<b>Здравей</b> &amp; &lt;b&gt;свят&lt;/b&gt;\n• <code>код</code>", f.sender.messages[0].Text)
}

// Telegram may still refuse a text it cannot parse: the same text goes again, as it is.
func TestAnAnswerTelegramCannotParseIsSentAgainAsPlainText(t *testing.T) {
	f := newFixture(42)
	f.processor.answer = "**Здравей**"
	f.sender.reject = func(m tgbotapi.MessageConfig) error {
		if m.ParseMode == tgbotapi.ModeHTML {
			return &tgbotapi.Error{Code: 400, Message: "Bad Request: can't parse entities: Unsupported start tag"}
		}
		return nil
	}

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Len(t, f.sender.attempts, 2, "the formatted try and the plain one")
	assert.Equal(t, tgbotapi.ModeHTML, f.sender.attempts[0].ParseMode)
	require.Len(t, f.sender.messages, 1)
	assert.Empty(t, f.sender.messages[0].ParseMode)
	assert.Equal(t, "**Здравей**", f.sender.messages[0].Text)
}

// Any other failure is not repeated: the message may have arrived, and a second one would be
// a duplicate.
func TestOtherFailuresOfAnAnswerAreNotSentAgain(t *testing.T) {
	failures := map[string]error{
		"network":         errors.New("connection reset"),
		"too long":        &tgbotapi.Error{Code: 400, Message: "Bad Request: message is too long"},
		"blocked by user": &tgbotapi.Error{Code: 403, Message: "Forbidden: bot was blocked by the user"},
		"parse in a 5xx":  &tgbotapi.Error{Code: 500, Message: "can't parse entities"},
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			f := newFixture(42)
			f.processor.answer = "**Здравей**"
			f.sender.reject = func(tgbotapi.MessageConfig) error { return failure }

			f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

			assert.Len(t, f.sender.attempts, 1, "one try only")
		})
	}
}

func TestRefusedMarkupIsRecognisedThroughWrapping(t *testing.T) {
	parse := &tgbotapi.Error{Code: 400, Message: "Bad Request: can't parse entities: Can't find end of the entity"}
	assert.True(t, refusedMarkup(parse))
	assert.True(t, refusedMarkup(fmt.Errorf("send: %w", parse)))
	assert.False(t, refusedMarkup(nil))
	assert.False(t, refusedMarkup(errors.New("can't parse entities")), "only the answer of Telegram counts")
	assert.False(t, refusedMarkup(&tgbotapi.Error{Code: 400, Message: "Bad Request: chat not found"}))
}

// A long answer goes out in pieces that each are a whole formatted message, and a code block
// that a cut falls into is closed and opened again.
func TestALongAnswerIsSentInFormattedPieces(t *testing.T) {
	f := newFixture(42)
	var code []string
	for i := 0; i < 700; i++ {
		code = append(code, fmt.Sprintf("line %d", i))
	}
	f.processor.answer = "**Начало**\n```\n" + strings.Join(code, "\n") + "\n```\nКрай"

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Greater(t, len(f.sender.messages), 1)
	for _, m := range f.sender.messages {
		assert.Equal(t, tgbotapi.ModeHTML, m.ParseMode)
		assert.NoError(t, wellFormed(m.Text))
		assert.LessOrEqual(t, utf8.RuneCountInString(m.Text), 4096)
	}
	assert.True(t, strings.HasPrefix(f.sender.messages[0].Text, "<b>Начало</b>\n<pre>line 0"))
	assert.True(t, strings.HasSuffix(f.sender.messages[len(f.sender.messages)-1].Text, "</pre>\nКрай"))
}

// What the bot says itself is not an answer of the model and is not parsed: a "*" in it stays.
// A text that converts to nothing (a bare code fence) is still said, as it was written.
func TestAnAnswerThatConvertsToNothingIsStillSent(t *testing.T) {
	f := newFixture(42)
	f.processor.answer = "```\n```"

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Len(t, f.sender.messages, 1)
	assert.Empty(t, f.sender.messages[0].ParseMode)
	assert.Equal(t, "```\n```", f.sender.messages[0].Text)
}

// The notice about a proposal is the bot's own text, with a command in it: not formatted.
func TestTheNoticeAboutAProposalIsPlainText(t *testing.T) {
	f := newFixture(42)
	f.processor.answer = "Предлагам правило."
	f.processor.proposals = []proposals.Proposal{{ID: 7, Kind: "rule", Key: "стил_на_писане", Value: "по-кратко"}}

	f.bot.handleMessage(context.Background(), privateMessage(42, "Говори по-кратко"))

	require.Len(t, f.sender.messages, 2)
	assert.Equal(t, tgbotapi.ModeHTML, f.sender.messages[0].ParseMode, "the answer")
	assert.Empty(t, f.sender.messages[1].ParseMode, "the notice")
	assert.Contains(t, f.sender.messages[1].Text, "/approve 7")
}

func TestTheBotsOwnWordsAreSentAsPlainText(t *testing.T) {
	f := newFixture(42)
	f.processor.err = errors.New("boom")

	f.bot.handleMessage(context.Background(), privateMessage(42, "Здравей"))

	require.Len(t, f.sender.messages, 1)
	assert.Empty(t, f.sender.messages[0].ParseMode)
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
	time.Sleep(20 * time.Millisecond) // let the second one queue behind the first turn of the chat

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

func (p *fakeProcessor) contents() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.reqs))
	for _, r := range p.reqs {
		out = append(out, r.Content)
	}
	return out
}

// Messages that arrive one after the other are answered in that order, even when
// they come faster than the model answers. The update loop (dispatch) is the one
// that sees the order, so the test goes through it.
func TestMessagesOfOneChatAreAnsweredInTheOrderTheyArrived(t *testing.T) {
	f := newFixture(42)
	release := make(chan struct{})
	f.processor.onCall = func(context.Context, uuid.UUID, agent.ChatRequest) { <-release }

	var want []string
	for i := 0; i < 12; i++ {
		text := fmt.Sprintf("въпрос %d", i)
		want = append(want, text)
		f.bot.dispatch(context.Background(), privateMessage(42, text))
	}
	close(release)
	f.bot.wg.Wait()

	assert.Equal(t, want, f.processor.contents())
}

// /id and a chat that is not allowed take no place in the queue: a place that
// nobody uses would hold up every later message of the chat.
func TestIDAndStrangersTakeNoPlaceInTheQueue(t *testing.T) {
	f := newFixture(42)

	assert.Nil(t, f.bot.reserveTurn(privateMessage(999, "Здравей")))
	assert.Nil(t, f.bot.reserveTurn(privateMessage(42, "/id")))
	assert.Nil(t, f.bot.reserveTurn(privateMessage(42, "/id@Lili_Smart_bot")))
	assert.Empty(t, f.bot.chatTails, "strangers must not grow the map")

	f.bot.dispatch(context.Background(), privateMessage(42, "/id"))
	f.bot.dispatch(context.Background(), privateMessage(42, "Здравей"))
	done := make(chan struct{})
	go func() { f.bot.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a message after /id is stuck in the queue")
	}
	assert.Equal(t, []string{"Здравей"}, f.processor.contents())
}

// A turn that gives up while it waits (its context is cancelled) must not let the
// turns behind it overtake the one that is still running.
func TestAGivenUpTurnKeepsTheOrderOfTheOthers(t *testing.T) {
	f := newFixture(42)
	firstRunning := make(chan struct{})
	finishFirst := make(chan struct{})
	var events []string
	var mu sync.Mutex
	note := func(s string) {
		mu.Lock()
		events = append(events, s)
		mu.Unlock()
	}
	f.processor.onCall = func(_ context.Context, _ uuid.UUID, req agent.ChatRequest) {
		note("start " + req.Content)
		if req.Content == "първи" {
			close(firstRunning)
			<-finishFirst
			note("end първи")
		}
	}

	first, second, third := privateMessage(42, "първи"), privateMessage(42, "втори"), privateMessage(42, "трети")
	t1, t2, t3 := f.bot.reserveTurn(first), f.bot.reserveTurn(second), f.bot.reserveTurn(third)
	go f.bot.handleMessageIn(context.Background(), first, t1)
	<-firstRunning
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan struct{})
	go func() { f.bot.handleMessageIn(secondCtx, second, t2); close(secondDone) }()
	thirdDone := make(chan struct{})
	go func() { f.bot.handleMessageIn(context.Background(), third, t3); close(thirdDone) }()

	cancelSecond()
	<-secondDone
	time.Sleep(50 * time.Millisecond) // the third must still be waiting for the first
	mu.Lock()
	assert.Equal(t, []string{"start първи"}, events, "the third turn must not start before the first has ended")
	mu.Unlock()

	close(finishFirst)
	select {
	case <-thirdDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the third turn never ran")
	}
	mu.Lock()
	assert.Equal(t, []string{"start първи", "end първи", "start трети"}, events)
	mu.Unlock()
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

// TELEGRAM_LLM=local: every turn tells the model router "the local model, even for a
// question to the library"; by default it says nothing and the router decides.
func TestTurnsCarryTheChoiceOfTheLocalModel(t *testing.T) {
	f := newFixture(42)
	f.bot.handleMessage(context.Background(), privateMessage(42, "Какво пише в книгата за стоиците?"))
	require.Len(t, f.processor.reqs, 1)
	assert.Nil(t, f.processor.reqs[0].UsePaid, "by default the router decides")

	f = newFixture(42)
	local := false
	f.bot.usePaid = &local
	f.bot.handleMessage(context.Background(), privateMessage(42, "Какво пише в книгата за стоиците?"))
	require.Len(t, f.processor.reqs, 1)
	require.NotNil(t, f.processor.reqs[0].UsePaid)
	assert.False(t, *f.processor.reqs[0].UsePaid, "TELEGRAM_LLM=local keeps the turn on the local model")
}

func TestLocalOnlyBecomesTheChoiceOfTheLocalModel(t *testing.T) {
	assert.Nil(t, usePaidFor(false))
	choice := usePaidFor(true)
	require.NotNil(t, choice)
	assert.False(t, *choice)
}

// ---------------------------------------------------- the answers given in the app
func mirrorFixture(chatID int64, user uuid.UUID) *fixture {
	f := newFixture(chatID)
	f.store.linked[chatID] = user
	return f
}

func TestAnAnswerGivenInTheAppIsRepeatedInTheLinkedChat(t *testing.T) {
	user := uuid.New()
	f := mirrorFixture(42, user)

	f.bot.Mirror(context.Background(), user, "tg-42", "Какво правиш?", "**Чета** книга", false)

	require.Len(t, f.sender.messages, 2)
	assert.Equal(t, int64(42), f.sender.messages[0].ChatID)
	assert.Equal(t, "📱 Ти (в приложението):\nКакво правиш?", f.sender.messages[0].Text)
	assert.Empty(t, f.sender.messages[0].ParseMode, "what the bot says itself is plain text")
	assert.Equal(t, "<b>Чета</b> книга", f.sender.messages[1].Text)
	assert.Equal(t, tgbotapi.ModeHTML, f.sender.messages[1].ParseMode, "the answer looks as any other answer")
}

func TestARegeneratedAnswerSaysSoAndDoesNotRepeatTheQuestion(t *testing.T) {
	user := uuid.New()
	f := mirrorFixture(42, user)

	f.bot.Mirror(context.Background(), user, "tg-42", "Въпрос", "Нов отговор", true)

	require.Len(t, f.sender.messages, 2)
	assert.Contains(t, f.sender.messages[0].Text, "Нов отговор на последния въпрос")
	assert.NotContains(t, f.sender.messages[0].Text, "Въпрос\n")
	assert.Equal(t, "Нов отговор", f.sender.messages[1].Text)
}

func TestALongQuestionIsCutInTheRepeat(t *testing.T) {
	user := uuid.New()
	f := mirrorFixture(42, user)

	f.bot.Mirror(context.Background(), user, "tg-42", strings.Repeat("я", 3000), "Отговор", false)

	require.Len(t, f.sender.messages, 2)
	assert.LessOrEqual(t, utf8.RuneCountInString(f.sender.messages[0].Text), 600)
	assert.True(t, strings.HasSuffix(f.sender.messages[0].Text, "…"))
}

// "tg-<number>" comes from a request: it must not be a way to write to a chat that is not the
// user's, or one the bot may not talk to.
func TestNothingIsRepeatedWhereItDoesNotBelong(t *testing.T) {
	user, stranger := uuid.New(), uuid.New()
	tests := []struct {
		name    string
		user    uuid.UUID
		session string
		allowed []int64
		linked  map[int64]uuid.UUID
	}{
		{"a chat the bot may not talk to", user, "tg-42", []int64{7}, map[int64]uuid.UUID{42: user}},
		{"a chat linked to another user", stranger, "tg-42", []int64{42}, map[int64]uuid.UUID{42: user}},
		{"a chat that is not linked", user, "tg-42", []int64{42}, map[int64]uuid.UUID{}},
		{"a session that is not Telegram's", user, "5f3a9c1e-7b2d-4c8a-9e61-0d4b7a2c8f13", []int64{42}, map[int64]uuid.UUID{42: user}},
		{"a session id that only looks like one", user, "tg-42abc", []int64{42}, map[int64]uuid.UUID{42: user}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(tc.allowed...)
			for chat, owner := range tc.linked {
				f.store.linked[chat] = owner
			}

			f.bot.Mirror(context.Background(), tc.user, tc.session, "Въпрос", "Отговор", false)

			assert.Empty(t, f.sender.attempts)
		})
	}
}

func TestAFailedLookupOfTheLinkRepeatsNothing(t *testing.T) {
	user := uuid.New()
	f := mirrorFixture(42, user)
	f.store.lookupErr = errors.New("db down")

	f.bot.Mirror(context.Background(), user, "tg-42", "Въпрос", "Отговор", false)

	assert.Empty(t, f.sender.attempts)
}
