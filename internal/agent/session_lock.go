package agent

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"go-monolith/metrics"
)

// ErrConversationBusy is returned when a turn waited for the conversation to be free and its
// time ran out first.
var ErrConversationBusy = errors.New("the conversation is busy with another reply")

// busyMessage is what a client that waited in vain is told.
const busyMessage = "Another reply in this conversation is still being written. Please try again in a moment."

// sessionLocks lets one conversation (a person and a session id) have one turn at a time.
// Without it two turns of the same conversation - one from the app and one from Telegram,
// which since plan item (4) share a conversation - read the same history, answer both, and
// the stored order is question, question, answer, answer. A turn that finds the conversation
// taken waits for its turn; the waiting turns are served in the order they came (a channel
// send queue is first in, first out).
//
// The zero value works. An entry lives only while a turn holds the lock or waits for it, so
// the map does not grow with the number of conversations.
type sessionLocks struct {
	mu sync.Mutex
	m  map[string]*sessionLock
}

type sessionLock struct {
	// token holds one value while a turn owns the conversation.
	token chan struct{}
	// users counts the turns that own the lock or wait for it.
	users int
}

// lock waits until the conversation is free and returns the function that frees it; call that
// on every path out of the turn (defer). waiting, if not nil, is called once, before the wait
// begins, and only when the conversation is taken. When ctx ends first the error is ctx's and
// nothing is held.
//
// The lock is for a turn that writes to the conversation. A background pass (the summary, the
// analysis of memories) and a tool must not take it: they run inside or after a turn, and one
// that waits for the turn it belongs to never ends.
func (s *sessionLocks) lock(ctx context.Context, userID uuid.UUID, sessionID string, waiting func()) (func(), error) {
	key := userID.String() + "|" + sessionID

	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[string]*sessionLock)
	}
	l := s.m[key]
	if l == nil {
		l = &sessionLock{token: make(chan struct{}, 1)}
		s.m[key] = l
	}
	l.users++
	s.mu.Unlock()

	leave := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		l.users--
		if l.users == 0 {
			delete(s.m, key)
		}
	}
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			<-l.token
			leave()
		})
	}

	select {
	case l.token <- struct{}{}:
		return unlock, nil
	default:
	}

	if waiting != nil {
		waiting()
	}
	started := time.Now()
	select {
	case l.token <- struct{}{}:
		metrics.ChatSessionWaits.WithLabelValues("served").Inc()
		metrics.ChatSessionWaitSeconds.Observe(time.Since(started).Seconds())
		return unlock, nil
	case <-ctx.Done():
		leave()
		metrics.ChatSessionWaits.WithLabelValues("gave_up").Inc()
		metrics.ChatSessionWaitSeconds.Observe(time.Since(started).Seconds())
		return nil, ctx.Err()
	}
}

// beginQueuedStream starts the event stream with the frame "queued" for a client that asked to
// be told it has to wait (queue_events). It reports whether the stream has begun: from then on
// a failure cannot be an HTTP status, only an "error" frame (see chatFailure).
func beginQueuedStream(w http.ResponseWriter) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, _ = w.Write([]byte("event: queued\ndata: {}\n\n"))
	flusher.Flush()
	return true
}

// chatFailure reports a failure of a turn: as an HTTP status while the response has not begun,
// as the terminal "error" frame once the stream has (streaming).
func chatFailure(w http.ResponseWriter, streaming bool, status int, message string) {
	if flusher, ok := w.(http.Flusher); ok && streaming {
		writeSSEError(w, flusher, message)
		return
	}
	http.Error(w, message, status)
}
