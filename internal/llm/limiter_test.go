package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type slotResult struct {
	release func()
	err     error
}

// acquireAsync starts an acquire in its own goroutine and returns the channel
// its outcome arrives on.
func acquireAsync(ctx context.Context, l *slotLimiter, background bool) <-chan slotResult {
	out := make(chan slotResult, 1)
	go func() {
		release, err := l.acquire(ctx, background)
		out <- slotResult{release: release, err: err}
	}()
	return out
}

// snapshot reads the limiter's counters.
func (l *slotLimiter) snapshot() (active, activeBG, waitingInteractive, waitingBackground int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.activeBG, len(l.interactiveQ), len(l.backgroundQ)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func mustGrant(t *testing.T, ch <-chan slotResult) func() {
	t.Helper()
	select {
	case res := <-ch:
		require.NoError(t, res.err)
		return res.release
	case <-time.After(3 * time.Second):
		t.Fatal("slot was not granted")
		return nil
	}
}

func mustWait(t *testing.T, ch <-chan slotResult) {
	t.Helper()
	select {
	case res := <-ch:
		t.Fatalf("slot was granted although none is free (err=%v)", res.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNewSlotLimiterClampsLimits(t *testing.T) {
	l := newSlotLimiter(0, 0)
	assert.Equal(t, 1, l.total)
	assert.Equal(t, 1, l.backgroundMax)

	l = newSlotLimiter(2, 5)
	assert.Equal(t, 2, l.total)
	assert.Equal(t, 2, l.backgroundMax, "background work cannot exceed the total")
}

func TestSlotLimiterRunsBackgroundWorkOneAtATime(t *testing.T) {
	l := newSlotLimiter(2, 1)
	ctx := context.Background()

	first := acquireAsync(ctx, l, true)
	releaseFirst := mustGrant(t, first)

	// Arrival order has to be fixed by the test: goroutines started back to back
	// may enqueue in either order.
	second := acquireAsync(ctx, l, true)
	eventually(t, "second request queued", func() bool {
		_, _, _, waiting := l.snapshot()
		return waiting == 1
	})
	third := acquireAsync(ctx, l, true)
	eventually(t, "third request queued", func() bool {
		_, _, _, waiting := l.snapshot()
		return waiting == 2
	})
	mustWait(t, second)
	mustWait(t, third)

	// Slots are handed on in arrival order.
	releaseFirst()
	releaseSecond := mustGrant(t, second)
	mustWait(t, third)

	releaseSecond()
	releaseThird := mustGrant(t, third)
	releaseThird()

	active, activeBG, _, _ := l.snapshot()
	assert.Zero(t, active)
	assert.Zero(t, activeBG)
}

func TestSlotLimiterInteractiveRequestDoesNotWaitBehindBackground(t *testing.T) {
	l := newSlotLimiter(2, 1)
	ctx := context.Background()

	releaseBackground := mustGrant(t, acquireAsync(ctx, l, true))
	queuedBackground := acquireAsync(ctx, l, true)
	eventually(t, "second background request queued", func() bool {
		_, _, _, waiting := l.snapshot()
		return waiting == 1
	})

	// The free slot is reserved for the user even though background work is waiting.
	releaseChat := mustGrant(t, acquireAsync(ctx, l, false))
	mustWait(t, queuedBackground)

	// Both slots are busy now, so a second chat request has to queue.
	secondChat := acquireAsync(ctx, l, false)
	mustWait(t, secondChat)

	releaseChat()
	releaseSecondChat := mustGrant(t, secondChat)

	releaseSecondChat()
	releaseBackground()
	releaseQueued := mustGrant(t, queuedBackground)
	releaseQueued()
}

func TestSlotLimiterPrefersInteractiveWaiterWhenSlotFrees(t *testing.T) {
	l := newSlotLimiter(2, 1)
	ctx := context.Background()

	releaseA := mustGrant(t, acquireAsync(ctx, l, false))
	releaseB := mustGrant(t, acquireAsync(ctx, l, false))

	// The background request arrives first but the chat request is served first.
	background := acquireAsync(ctx, l, true)
	eventually(t, "background request queued", func() bool {
		_, _, _, waiting := l.snapshot()
		return waiting == 1
	})
	chat := acquireAsync(ctx, l, false)
	eventually(t, "chat request queued", func() bool {
		_, _, waiting, _ := l.snapshot()
		return waiting == 1
	})

	releaseA()
	releaseChat := mustGrant(t, chat)
	mustWait(t, background)

	releaseB()
	releaseBackground := mustGrant(t, background)

	releaseChat()
	releaseBackground()
}

func TestSlotLimiterCancelledWaiterLeavesQueueAndLeaksNothing(t *testing.T) {
	l := newSlotLimiter(1, 1)
	releaseHolder := mustGrant(t, acquireAsync(context.Background(), l, false))

	ctx, cancel := context.WithCancel(context.Background())
	waiting := acquireAsync(ctx, l, true)
	eventually(t, "request queued", func() bool {
		_, _, _, queued := l.snapshot()
		return queued == 1
	})

	cancel()
	select {
	case res := <-waiting:
		assert.ErrorIs(t, res.err, context.Canceled)
		assert.Nil(t, res.release)
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	active, _, _, queued := l.snapshot()
	assert.Equal(t, 1, active, "only the holder is running")
	assert.Zero(t, queued, "the cancelled request left the queue")

	releaseHolder()
	releaseNext := mustGrant(t, acquireAsync(context.Background(), l, false))
	releaseNext()
	active, _, _, _ = l.snapshot()
	assert.Zero(t, active)
}

// lateCancelCtx reports no error on the first Err call but has a closed Done
// channel, i.e. it is cancelled the instant acquire starts waiting. That makes
// the race between "slot granted" and "context done" deterministic: both select
// cases are ready and Go picks one at random.
type lateCancelCtx struct {
	context.Context
	done    chan struct{}
	errCall atomic.Int32
}

func newLateCancelCtx() *lateCancelCtx {
	done := make(chan struct{})
	close(done)
	return &lateCancelCtx{Context: context.Background(), done: done}
}

func (c *lateCancelCtx) Done() <-chan struct{} { return c.done }

func (c *lateCancelCtx) Err() error {
	if c.errCall.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func TestSlotLimiterGivesBackSlotGrantedAsContextEnds(t *testing.T) {
	l := newSlotLimiter(2, 1)

	granted, cancelled := 0, 0
	for i := 0; i < 200; i++ {
		release, err := l.acquire(newLateCancelCtx(), i%2 == 0)
		if err == nil {
			granted++
			release()
		} else {
			cancelled++
			assert.ErrorIs(t, err, context.Canceled)
		}
	}

	require.NotZero(t, granted, "the test must exercise both outcomes of the race")
	require.NotZero(t, cancelled, "the test must exercise both outcomes of the race")
	active, activeBG, _, _ := l.snapshot()
	assert.Zero(t, active, "a slot granted to a request that gave up must be handed back")
	assert.Zero(t, activeBG)
}

func TestSlotLimiterRefusesAlreadyCancelledContext(t *testing.T) {
	l := newSlotLimiter(2, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := l.acquire(ctx, false)
	assert.ErrorIs(t, err, context.Canceled)

	active, _, waitingInteractive, _ := l.snapshot()
	assert.Zero(t, active)
	assert.Zero(t, waitingInteractive)
}

func TestSlotLimiterReleaseIsIdempotent(t *testing.T) {
	l := newSlotLimiter(2, 1)

	release := mustGrant(t, acquireAsync(context.Background(), l, true))
	release()
	release()

	active, activeBG, _, _ := l.snapshot()
	assert.Zero(t, active, "a second release must not free someone else's slot")
	assert.Zero(t, activeBG)
}

func TestWithBackgroundMarksContext(t *testing.T) {
	assert.False(t, IsBackground(context.Background()))
	assert.True(t, IsBackground(WithBackground(context.Background())))
}

// TestSlotLimiterNeverExceedsLimits hammers the limiter and checks the two
// invariants it exists for.
func TestSlotLimiterNeverExceedsLimits(t *testing.T) {
	const total, backgroundMax = 3, 1
	l := newSlotLimiter(total, backgroundMax)

	var running, runningBG, maxRunning, maxRunningBG atomic.Int32
	bump := func(current int32, peak *atomic.Int32) {
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				return
			}
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		background := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.acquire(context.Background(), background)
			if err != nil {
				t.Error(err)
				return
			}

			bump(running.Add(1), &maxRunning)
			if background {
				bump(runningBG.Add(1), &maxRunningBG)
			}
			time.Sleep(time.Millisecond)
			if background {
				runningBG.Add(-1)
			}
			running.Add(-1)
			release()
		}()
	}
	wg.Wait()

	assert.LessOrEqual(t, int(maxRunning.Load()), total)
	assert.LessOrEqual(t, int(maxRunningBG.Load()), backgroundMax)
	active, activeBG, _, _ := l.snapshot()
	assert.Zero(t, active)
	assert.Zero(t, activeBG)
}
