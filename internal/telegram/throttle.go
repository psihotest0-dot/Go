package telegram

import (
	"sync"
	"time"
)

// throttle lets one thing through per id at most once every `every`. It is used for
// the chats that are not allowed: anybody who finds the bot can write to it, so what
// they cost (a reply, a log line) must not grow with how much they write.
//
// The table is bounded: when it is full, the entries that have expired are dropped, and
// if none have, a new id is refused until some do. The zero value is not ready; use
// newThrottle.
type throttle struct {
	mu    sync.Mutex
	every time.Duration
	max   int
	last  map[int64]time.Time
}

func newThrottle(every time.Duration, max int) *throttle {
	return &throttle{every: every, max: max, last: make(map[int64]time.Time)}
}

// allow reports whether id may pass at time now, and counts it if so.
func (t *throttle) allow(id int64, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if at, ok := t.last[id]; ok {
		if now.Sub(at) < t.every {
			return false
		}
		t.last[id] = now
		return true
	}
	if len(t.last) >= t.max {
		for k, at := range t.last {
			if now.Sub(at) >= t.every {
				delete(t.last, k)
			}
		}
		if len(t.last) >= t.max {
			return false
		}
	}
	t.last[id] = now
	return true
}
