package llm

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go-monolith/metrics"
)

const (
	priorityInteractive = "interactive"
	priorityBackground  = "background"

	// slowSlotWait is how long a request may wait for a slot before the wait is
	// worth a log line.
	slowSlotWait = 5 * time.Second
)

type backgroundKey struct{}

// WithBackground marks ctx as belonging to background work (rolling summary,
// memory consolidation, distillation). Requests made with it may use only a
// limited share of the model's slots, so a long background job cannot make a
// chat turn wait.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx was marked with WithBackground.
func IsBackground(ctx context.Context) bool {
	background, _ := ctx.Value(backgroundKey{}).(bool)
	return background
}

type slotWaiter struct {
	background bool
	granted    bool
	ready      chan struct{}
}

// slotLimiter hands out the model's parallel slots. Interactive requests may
// take any slot; background requests at most backgroundMax of them and only when
// no interactive request is waiting, so one slot stays free for a user. Waiters
// of the same kind are served in arrival order.
type slotLimiter struct {
	mu            sync.Mutex
	total         int
	backgroundMax int
	active        int
	activeBG      int
	interactiveQ  []*slotWaiter
	backgroundQ   []*slotWaiter
}

// newSlotLimiter builds a limiter with total slots, backgroundMax of which may
// be used by background work. Both are clamped to at least 1 (a background limit
// of 0 would starve background work forever) and backgroundMax to at most total.
func newSlotLimiter(total, backgroundMax int) *slotLimiter {
	if total < 1 {
		total = 1
	}
	if backgroundMax < 1 {
		backgroundMax = 1
	}
	if backgroundMax > total {
		backgroundMax = total
	}
	return &slotLimiter{total: total, backgroundMax: backgroundMax}
}

// acquire blocks until a slot is free or ctx is done. The returned function
// gives the slot back and is safe to call more than once.
func (l *slotLimiter) acquire(ctx context.Context, background bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	priority := priorityInteractive
	if background {
		priority = priorityBackground
	}

	start := time.Now()
	w := &slotWaiter{background: background, ready: make(chan struct{})}

	l.mu.Lock()
	if background {
		l.backgroundQ = append(l.backgroundQ, w)
	} else {
		l.interactiveQ = append(l.interactiveQ, w)
	}
	l.dispatchLocked()
	l.mu.Unlock()

	select {
	case <-w.ready:
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			// The slot was handed over in the same instant ctx ended: give it
			// back, otherwise it would be lost for good.
			l.releaseLocked(w.background)
			l.dispatchLocked()
		} else {
			l.removeLocked(w)
		}
		l.mu.Unlock()
		return nil, ctx.Err()
	}

	waited := time.Since(start)
	metrics.LLMQueueWait.WithLabelValues(priority).Observe(waited.Seconds())
	if waited >= slowSlotWait {
		slog.Info("LLM request waited for a free slot", "priority", priority, "waited", waited.Round(time.Millisecond))
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.releaseLocked(background)
			l.dispatchLocked()
			l.mu.Unlock()
		})
	}, nil
}

func (l *slotLimiter) releaseLocked(background bool) {
	l.active--
	if background {
		l.activeBG--
	}
}

// dispatchLocked grants free slots to waiting requests: interactive first.
func (l *slotLimiter) dispatchLocked() {
	for l.active < l.total {
		switch {
		case len(l.interactiveQ) > 0:
			w := l.interactiveQ[0]
			l.interactiveQ = l.interactiveQ[1:]
			l.grantLocked(w)
		case len(l.backgroundQ) > 0 && l.activeBG < l.backgroundMax:
			w := l.backgroundQ[0]
			l.backgroundQ = l.backgroundQ[1:]
			l.grantLocked(w)
		default:
			return
		}
	}
}

func (l *slotLimiter) grantLocked(w *slotWaiter) {
	l.active++
	if w.background {
		l.activeBG++
	}
	w.granted = true
	close(w.ready)
}

func (l *slotLimiter) removeLocked(w *slotWaiter) {
	queue := &l.interactiveQ
	if w.background {
		queue = &l.backgroundQ
	}
	for i, queued := range *queue {
		if queued == w {
			*queue = append((*queue)[:i], (*queue)[i+1:]...)
			return
		}
	}
}
