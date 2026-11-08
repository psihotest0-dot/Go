package telegram

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// The Telegram Bot API carries the token in the URL path
// (https://api.telegram.org/bot<TOKEN>/getUpdates), so every transport error
// from net/http (*url.Error) contains it. Without the code below the token
// reached the terminal on every network drop: the library prints each failed
// long-poll itself, and Bot.send logs a failed reply at ERROR level, which also
// goes to stdout.

const redacted = "<redacted>"

// libraryLogRepeatWindow is how long an identical library line is logged only
// once. While the network is down the long-poll fails every 3 seconds.
const libraryLogRepeatWindow = time.Minute

// maxRememberedLibraryLines bounds the memory of the repeat filter.
const maxRememberedLibraryLines = 64

// redactToken removes the token from an error. A *url.Error keeps its type and
// its Timeout/Unwrap behaviour; only the URL (and, if needed, the cause) change.
func redactToken(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	if ue, ok := err.(*url.Error); ok {
		return &url.Error{
			Op:  ue.Op,
			URL: strings.ReplaceAll(ue.URL, token, redacted),
			Err: redactToken(ue.Err, token),
		}
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, redacted))
}

// redactingClient is the HTTP client of the Telegram library: it hands out
// errors without the token, wherever the library or this package logs them.
type redactingClient struct {
	next  tgbotapi.HTTPClient
	token string
}

func newRedactingClient(next *http.Client, token string) *redactingClient {
	return &redactingClient{next: next, token: token}
}

func (c *redactingClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.next.Do(req)
	return resp, redactToken(err, c.token)
}

// libraryLogger routes the library's own log lines (tgbotapi.SetLogger) to slog
// at WARN, so they land in backend.log rather than raw on stderr, without the
// token and without the same line every 3 seconds during an outage.
type libraryLogger struct {
	token string
	now   func() time.Time

	mu   sync.Mutex
	seen map[string]seenLine
}

type seenLine struct {
	at         time.Time
	suppressed int
}

func newLibraryLogger(token string) *libraryLogger {
	return &libraryLogger{token: token, now: time.Now, seen: map[string]seenLine{}}
}

func (l *libraryLogger) Println(v ...any) {
	l.log(strings.TrimSuffix(fmt.Sprintln(v...), "\n"))
}

func (l *libraryLogger) Printf(format string, v ...any) {
	l.log(strings.TrimSuffix(fmt.Sprintf(format, v...), "\n"))
}

func (l *libraryLogger) log(line string) {
	if l.token != "" {
		line = strings.ReplaceAll(line, l.token, redacted)
	}

	l.mu.Lock()
	now := l.now()
	prev, known := l.seen[line]
	if known && now.Sub(prev.at) < libraryLogRepeatWindow {
		prev.suppressed++
		l.seen[line] = prev
		l.mu.Unlock()
		return
	}
	if len(l.seen) >= maxRememberedLibraryLines {
		clear(l.seen)
	}
	l.seen[line] = seenLine{at: now}
	l.mu.Unlock()

	if prev.suppressed > 0 {
		slog.Warn("telegram library", slog.String("message", line), slog.Int("repeats_suppressed", prev.suppressed))
		return
	}
	slog.Warn("telegram library", slog.String("message", line))
}
