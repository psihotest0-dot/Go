package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "123456:SECRET-token-value"

// lockedBuffer is a log sink the library's goroutine may write to while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog replaces the default logger for the test.
func captureSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// deadTelegram is a Bot API that answers getMe once and is then shut down, so
// every later call fails the way a dropped network does. The URL carries the
// token like the real one: <server>/bot<token>/<method>.
func deadTelegram(t *testing.T) (endpoint string, stop func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"t","username":"t_bot"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/bot%s/%s", srv.Close
}

func TestRedactTokenKeepsTheErrorUsable(t *testing.T) {
	cause := context.DeadlineExceeded
	err := &url.Error{Op: "Post", URL: "https://api.telegram.org/bot" + testToken + "/getUpdates", Err: cause}

	got := redactToken(err, testToken)

	assert.NotContains(t, got.Error(), testToken)
	assert.Contains(t, got.Error(), "https://api.telegram.org/bot<redacted>/getUpdates")
	var ue *url.Error
	require.ErrorAs(t, got, &ue, "still a *url.Error")
	assert.ErrorIs(t, got, context.DeadlineExceeded, "the cause is kept")
	assert.True(t, ue.Timeout(), "timeouts are still recognised")
	assert.Contains(t, err.Error(), testToken, "the original error is not modified")
}

func TestRedactTokenCoversEveryKindOfError(t *testing.T) {
	plain := errors.New("proxy said no for https://api.telegram.org/bot" + testToken + "/send")
	assert.NotContains(t, redactToken(plain, testToken).Error(), testToken)

	nested := &url.Error{Op: "Get", URL: "https://x/bot" + testToken, Err: plain}
	assert.NotContains(t, redactToken(nested, testToken).Error(), testToken)

	clean := errors.New("connection refused")
	assert.Same(t, clean, redactToken(clean, testToken), "an error without the token is returned as it is")
	assert.NoError(t, redactToken(nil, testToken))

	// An empty token must not turn into "replace the empty string everywhere".
	assert.Same(t, clean, redactToken(clean, ""))
}

// Against a real transport: the plain client leaks the token, the wrapped one
// does not. Without the first assertion the test could pass for the wrong reason.
func TestTransportErrorsDoNotCarryTheToken(t *testing.T) {
	endpoint, stop := deadTelegram(t)
	api, err := newAPI(testToken, endpoint)
	require.NoError(t, err)
	stop()

	_, plainErr := (&http.Client{Timeout: time.Second}).Post(fmt.Sprintf(endpoint, testToken, "sendMessage"), "text/plain", nil)
	require.Error(t, plainErr)
	require.Contains(t, plainErr.Error(), testToken, "the premise: net/http puts the token in its errors")

	_, err = api.Send(tgbotapi.NewMessage(1, "hi"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
	assert.Contains(t, err.Error(), "<redacted>")
}

func TestNewAPIFailureDoesNotCarryTheToken(t *testing.T) {
	endpoint, stop := deadTelegram(t)
	stop()

	_, err := newAPI(testToken, endpoint)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
}

// The library's long-poll loop prints a failed request itself. That line must
// reach slog (backend.log) at WARN and without the token.
func TestLongPollFailuresAreLoggedWithoutTheToken(t *testing.T) {
	endpoint, stop := deadTelegram(t)
	api, err := newAPI(testToken, endpoint)
	require.NoError(t, err)
	logs := captureSlog(t)
	stop()

	api.GetUpdatesChan(tgbotapi.NewUpdate(0))
	// Both lines of one failure are logged before the loop sleeps for 3 seconds.
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "retrying in 3 seconds")
	}, 5*time.Second, 20*time.Millisecond)
	api.StopReceivingUpdates()

	out := logs.String()
	assert.NotContains(t, out, testToken)
	assert.Contains(t, out, `"level":"WARN"`)
	assert.Contains(t, out, "getUpdates")
	assert.Contains(t, out, "<redacted>")
}

func TestLibraryLoggerRemovesTheTokenFromAnyLine(t *testing.T) {
	logs := captureSlog(t)
	l := newLibraryLogger(testToken)

	l.Println(errors.New(`Post "https://api.telegram.org/bot` + testToken + `/getUpdates": EOF`))
	l.Printf("Endpoint: %s, response: %s\n", "bot"+testToken, "{}")

	out := logs.String()
	assert.NotContains(t, out, testToken)
	assert.Equal(t, 2, strings.Count(out, `"level":"WARN"`))
}

func TestLibraryLoggerThrottlesIdenticalLines(t *testing.T) {
	logs := captureSlog(t)
	l := newLibraryLogger(testToken)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	// An outage: the same line every 3 seconds for a minute and a half.
	for i := 0; i < 30; i++ {
		l.Println("Failed to get updates, retrying in 3 seconds...")
		now = now.Add(3 * time.Second)
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	assert.Len(t, lines, 2, "once at the start and once after the window")
	assert.NotContains(t, lines[0], "repeats_suppressed")
	assert.Contains(t, lines[1], `"repeats_suppressed":19`)

	l.Println("Stopping the update receiver routine...")
	assert.Equal(t, 3, strings.Count(strings.TrimSpace(logs.String()), "\n")+1, "a different line is never suppressed")
}

func TestLibraryLoggerMemoryIsBounded(t *testing.T) {
	captureSlog(t)
	l := newLibraryLogger(testToken)

	for i := 0; i < 10*maxRememberedLibraryLines; i++ {
		l.Println(fmt.Sprintf("dial tcp 127.0.0.1:%d: connection refused", i))
	}

	assert.LessOrEqual(t, len(l.seen), maxRememberedLibraryLines)
}
