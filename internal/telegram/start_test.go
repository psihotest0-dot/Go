package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/agent"
)

// fakeBotAPI is a Telegram Bot API server for one chat: getMe, a getUpdates that hands
// over the queued updates once and then idles like a long poll, sendMessage and
// sendChatAction, which it records in the order they arrive.
type fakeBotAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	pending []map[string]any
	events  []string // "typing" and "message: <text>", in the order they arrived

	// refuseHTML makes sendMessage with parse_mode=HTML fail the way Telegram does for markup
	// it cannot read; every sendMessage, refused or not, leaves its parse_mode in modes.
	refuseHTML bool
	modes      []string
}

func newFakeBotAPI(t *testing.T, texts ...string) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{}
	for i, text := range texts {
		f.pending = append(f.pending, map[string]any{
			"update_id": i + 1,
			"message": map[string]any{
				"message_id": i + 1, "date": 0, "text": text,
				"chat": map[string]any{"id": 42, "type": "private"},
				"from": map[string]any{"id": 42, "is_bot": false, "first_name": "Owner"},
			},
		})
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	reply := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}) }
	switch method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]; method {
	case "getMe":
		reply(map[string]any{"id": 1, "is_bot": true, "first_name": "Test", "username": "test_bot"})
	case "getUpdates":
		f.mu.Lock()
		updates := f.pending
		f.pending = nil
		f.mu.Unlock()
		if len(updates) == 0 {
			select {
			case <-time.After(30 * time.Millisecond):
			case <-r.Context().Done():
			}
			reply([]any{})
			return
		}
		reply(updates)
	case "sendChatAction":
		f.mu.Lock()
		f.events = append(f.events, "typing")
		f.mu.Unlock()
		reply(true)
	case "sendMessage":
		mode := r.Form.Get("parse_mode")
		f.mu.Lock()
		f.modes = append(f.modes, mode)
		refuse := f.refuseHTML && mode == "HTML"
		if !refuse {
			f.events = append(f.events, "message: "+r.Form.Get("text"))
		}
		f.mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 400, "description": "Bad Request: can't parse entities: Unsupported start tag",
			})
			return
		}
		reply(map[string]any{"message_id": 100, "date": 0, "chat": map[string]any{"id": 42, "type": "private"}, "text": r.Form.Get("text")})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeBotAPI) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeBotAPI) messages() []string {
	var out []string
	for _, e := range f.log() {
		if text, ok := strings.CutPrefix(e, "message: "); ok {
			out = append(out, text)
		}
	}
	return out
}

// slowFirst answers "reply to <text>", the first question slowly: the second would be
// ready long before it, which is what a swapped order needs.
type slowFirst struct {
	mu    sync.Mutex
	calls []string
}

func (p *slowFirst) ProcessChatTurn(_ context.Context, _ uuid.UUID, req agent.ChatRequest) (agent.ChatTurn, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req.Content)
	first := len(p.calls) == 1
	p.mu.Unlock()
	if first {
		time.Sleep(300 * time.Millisecond)
	}
	return agent.ChatTurn{Answer: "reply to " + req.Content}, nil
}

// The real loop, the real library and a fake Telegram: two messages that arrive in one
// batch are answered in that order although the first takes much longer (plan items P12
// and "two quick messages" of 2026-11-19), and the "typing..." action is sent before the
// reply of each.
func TestTheUpdateLoopAnswersQuickMessagesInOrderAndShowsTyping(t *testing.T) {
	api := newFakeBotAPI(t, "едно", "две")
	bot, err := newAPI("123:TEST-TOKEN", api.srv.URL+"/bot%s/%s")
	require.NoError(t, err)
	b := newBot(bot, &slowFirst{}, newFakeStore(), newFakeDesk(), []int64{42})
	b.api = bot

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Start(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after the cancel")
		}
	}()

	require.Eventually(t, func() bool { return len(api.messages()) == 2 }, 10*time.Second, 20*time.Millisecond,
		"both replies arrive; got %v", api.log())

	assert.Equal(t, []string{"reply to едно", "reply to две"}, api.messages(), "in the order the messages arrived")
	events := api.log()
	require.GreaterOrEqual(t, len(events), 3)
	assert.Equal(t, "typing", events[0], "the first thing in the chat is the typing indicator: %v", events)
	firstReply := indexOf(events, "message: reply to едно")
	assert.Positive(t, firstReply, "typing came before the first reply: %v", events)
	assert.Equal(t, []string{"typing"}, events[:firstReply], "one typing indicator while the first reply is awaited (it repeats only every few seconds): %v", events)
}

// The real library meets the real shape of Telegram's refusal: the error it returns is the one
// refusedMarkup knows, so the answer is sent again as plain text instead of being lost.
func TestTheRealClientTurnsTelegramsRefusalOfMarkupIntoPlainText(t *testing.T) {
	api := newFakeBotAPI(t, "здравей")
	api.refuseHTML = true
	bot, err := newAPI("123:TEST-TOKEN", api.srv.URL+"/bot%s/%s")
	require.NoError(t, err)
	b := newBot(bot, &fakeProcessor{answer: "**важно** <b>"}, newFakeStore(), newFakeDesk(), []int64{42})
	b.api = bot

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Start(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after the cancel")
		}
	}()

	require.Eventually(t, func() bool { return len(api.messages()) == 1 }, 10*time.Second, 20*time.Millisecond,
		"the plain text arrives; got %v", api.log())

	assert.Equal(t, []string{"**важно** <b>"}, api.messages(), "the answer as the model wrote it")
	api.mu.Lock()
	defer api.mu.Unlock()
	assert.Equal(t, []string{"HTML", ""}, api.modes, "the formatted try, then the plain one")
}

func indexOf(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}
