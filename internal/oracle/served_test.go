package oracle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// asIn is the request the bot sends for the same signal, in the given mode. The symbol is not
// the one of the fixture, so that a row that took the symbol from somewhere else shows.
func asIn(t *testing.T, mode string) (*Request, []byte) {
	t.Helper()
	return mustDecode(t, func(m map[string]any) {
		m["mode"] = mode
		m["signal"].(map[string]any)["symbol"] = "KO"
	})
}

// The bot asks one signal in two modes (shadow for the evaluation, active from its paper
// container). Whichever comes second is answered from the first, and used to leave no row, so
// the journal said the signal was asked in one mode only.
func TestTheSecondModeToAskASignalLeavesALightRow(t *testing.T) {
	for _, tc := range []struct{ first, second string }{
		{ModeShadow, ModeActive},
		{ModeActive, ModeShadow},
	} {
		t.Run(tc.first+" then "+tc.second, func(t *testing.T) {
			model := &fakeLLM{replies: []reply{good(VerdictApprove, "Трендът е нагоре.")}}
			svc, store, _ := newTestService(model)
			req1, raw1 := asIn(t, tc.first)
			req2, raw2 := asIn(t, tc.second)

			first, err := svc.Evaluate(context.Background(), req1, raw1, "req-1")
			require.NoError(t, err)
			second, err := svc.Evaluate(context.Background(), req2, raw2, "req-2")
			require.NoError(t, err)
			waitServed(svc)

			assert.Equal(t, 1, model.callCount(), "the model is asked once")
			assert.True(t, second.Cached)
			rows := store.snapshot()
			require.Len(t, rows, 2)

			asked, served := rows[0], rows[1]
			assert.Equal(t, tc.first, asked.Mode)
			assert.Empty(t, asked.ServedFrom, "the row of the call that reached the model is as it always was")
			assert.Equal(t, 1, asked.Attempts)

			assert.Equal(t, tc.second, served.Mode, "the mode of the call that was answered, not of the one that asked")
			assert.Equal(t, ServedFromCache, served.ServedFrom)
			assert.Equal(t, StatusAnswered, served.Status)
			assert.Equal(t, first.Verdict, served.Verdict)
			assert.Equal(t, first.Reason, served.Reason)
			assert.Equal(t, "test-model", served.Model)
			assert.Equal(t, first.PromptVersion, served.PromptVersion)
			assert.NotEmpty(t, served.PromptVersion, "the label of the question that produced the verdict")
			assert.Equal(t, "req-2", served.RequestID, "the id of the call the row is about")
			assert.Equal(t, "sig-0001", served.SignalID)
			assert.Equal(t, "KO", served.Symbol)
			assert.JSONEq(t, string(raw2), string(served.Request), "the body of this call")
			assert.Zero(t, served.Attempts)
			assert.Zero(t, served.PromptTokens)
			assert.Zero(t, served.CompletionTokens)
			assert.Zero(t, served.LatencyMS, "no time was spent on the model")
			assert.Empty(t, served.FinishReason)
			assert.Empty(t, served.Error)
		})
	}
}

// The bot asks a HOLD again every minute. A repeat in the mode that already has a row is not
// a row; a repeat in the other mode is one row, however many times it comes.
func TestAtMostOneLightRowForASignalAndMode(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictHold, "Чака се обем.")}}
	svc, store, clk := newTestService(model)
	shadow, shadowRaw := asIn(t, ModeShadow)
	active, activeRaw := asIn(t, ModeActive)

	_, err := svc.Evaluate(context.Background(), shadow, shadowRaw, "s1")
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		clk.Advance(20 * time.Second) // inside the HOLD window: every ask is answered from memory
		_, err = svc.Evaluate(context.Background(), active, activeRaw, "a"+string(rune('1'+i)))
		require.NoError(t, err)
		_, err = svc.Evaluate(context.Background(), shadow, shadowRaw, "s"+string(rune('2'+i)))
		require.NoError(t, err)
		waitServed(svc)
	}

	assert.Equal(t, 1, model.callCount())
	rows := store.snapshot()
	require.Len(t, rows, 2, "one row of the shadow call that reached the model, one light row of the active one")
	assert.Equal(t, ModeShadow, rows[0].Mode)
	assert.Equal(t, ModeActive, rows[1].Mode)
	assert.Equal(t, "a1", rows[1].RequestID, "the first of the repeats is the one that is kept")
}

// The same mode asking again after the verdict is cached never adds a row: this is what the
// journal did before, and it is what keeps the shadow counters of the bot's judge one row per ask.
func TestARepeatInTheSameModeIsStillNoRow(t *testing.T) {
	for _, mode := range []string{ModeShadow, ModeActive, ""} {
		t.Run("mode "+mode, func(t *testing.T) {
			model := &fakeLLM{replies: []reply{good(VerdictVeto, "Не.")}}
			svc, store, _ := newTestService(model)
			req, raw := asIn(t, mode)

			for i := 0; i < 3; i++ {
				_, err := svc.Evaluate(context.Background(), req, raw, "r")
				require.NoError(t, err)
			}
			waitServed(svc)

			assert.Len(t, store.snapshot(), 1)
		})
	}
}

// A call that does not say its mode is a mode of its own: a call in a mode that has no row yet
// gets one, and a call with none is not mistaken for shadow or for active.
func TestACallWithoutAModeIsAModeOfItsOwn(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "Да.")}}
	svc, store, _ := newTestService(model)
	shadow, shadowRaw := asIn(t, ModeShadow)
	none, noneRaw := asIn(t, "")

	_, err := svc.Evaluate(context.Background(), shadow, shadowRaw, "s")
	require.NoError(t, err)
	_, err = svc.Evaluate(context.Background(), none, noneRaw, "n")
	require.NoError(t, err)
	waitServed(svc)

	rows := store.snapshot()
	require.Len(t, rows, 2)
	assert.Equal(t, "", rows[1].Mode)
	assert.Equal(t, ServedFromCache, rows[1].ServedFrom)
}

// Two modes ask at the same moment: the second waits for the first call to the model and gets
// a row of its own mode, marked "joined".
func TestACallThatJoinsAnotherLeavesALightRowMarkedJoined(t *testing.T) {
	model := &fakeLLM{
		replies: []reply{good(VerdictApprove, "Да.")},
		gate:    make(chan struct{}),
		started: make(chan struct{}, 1),
	}
	svc, store, _ := newTestService(model)
	shadow, shadowRaw := asIn(t, ModeShadow)
	active, activeRaw := asIn(t, ModeActive)
	cachedActive, cachedShadow := requestsIn(t, "cached", "active"), requestsIn(t, "cached", "shadow")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := svc.Evaluate(context.Background(), shadow, shadowRaw, "first")
		assert.NoError(t, err)
	}()
	<-model.started // the first call is at the model
	var res Result
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		res, err = svc.Evaluate(context.Background(), active, activeRaw, "second")
		assert.NoError(t, err)
	}()
	time.Sleep(200 * time.Millisecond) // the second call has joined the first
	close(model.gate)
	wg.Wait()
	waitServed(svc)

	assert.True(t, res.Cached)
	assert.Equal(t, 1, model.callCount())
	assert.Equal(t, cachedActive+1, requestsIn(t, "cached", "active"), "a call that joined is counted in its own mode")
	assert.Equal(t, cachedShadow, requestsIn(t, "cached", "shadow"))
	rows := store.snapshot()
	require.Len(t, rows, 2)
	assert.Equal(t, ModeShadow, rows[0].Mode)
	assert.Empty(t, rows[0].ServedFrom)
	assert.Equal(t, ModeActive, rows[1].Mode)
	assert.Equal(t, ServedFromJoined, rows[1].ServedFrom)
	assert.Equal(t, "second", rows[1].RequestID)
	assert.Equal(t, VerdictApprove, rows[1].Verdict)
}

// The route answers at once: the row is written in the background, so a slow or stuck database
// does not slow the bot's call (and with it the availability the evaluation counts).
func TestTheLightRowDoesNotHoldTheAnswer(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "Да.")}}
	svc, mem, _ := newTestService(model)
	release := make(chan struct{})
	svc.store = &blockedServedStore{memStore: mem, release: release}
	shadow, shadowRaw := asIn(t, ModeShadow)
	active, activeRaw := asIn(t, ModeActive)

	_, err := svc.Evaluate(context.Background(), shadow, shadowRaw, "s")
	require.NoError(t, err)

	done := make(chan Result, 1)
	go func() {
		res, err := svc.Evaluate(context.Background(), active, activeRaw, "a")
		assert.NoError(t, err)
		done <- res
	}()
	select {
	case res := <-done:
		assert.True(t, res.Cached)
	case <-time.After(2 * time.Second):
		t.Fatal("the answer waited for the journal")
	}
	assert.Len(t, mem.snapshot(), 1, "the light row is not written yet")

	close(release)
	waitServed(svc)
	assert.Len(t, mem.snapshot(), 2)
}

// blockedServedStore holds SaveServed until release is closed, then writes only if the context
// it was given is still alive (the database driver would refuse a cancelled one too).
type blockedServedStore struct {
	*memStore
	release chan struct{}
}

func (b *blockedServedStore) SaveServed(ctx context.Context, e Evaluation) (bool, error) {
	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		return false, errors.New("the test never released the write")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return b.memStore.SaveServed(ctx, e)
}

// A row that cannot be written is a log line, not a lost answer.
func TestALostLightRowDoesNotLoseTheAnswer(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictVeto, "Не.")}}
	svc, store, _ := newTestService(model)
	shadow, shadowRaw := asIn(t, ModeShadow)
	active, activeRaw := asIn(t, ModeActive)

	_, err := svc.Evaluate(context.Background(), shadow, shadowRaw, "s")
	require.NoError(t, err)
	store.mu.Lock()
	store.failSave = true
	store.mu.Unlock()

	res, err := svc.Evaluate(context.Background(), active, activeRaw, "a")
	waitServed(svc)

	require.NoError(t, err)
	assert.Equal(t, VerdictVeto, res.Verdict)
	assert.True(t, res.Cached)
	assert.Len(t, store.snapshot(), 1)
}

// A caller that has gone away does not cancel the write: the row is about a call that was answered.
func TestTheLightRowIsWrittenEvenWhenTheCallerLeaves(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "Да.")}}
	svc, store, _ := newTestService(model)
	release := make(chan struct{})
	svc.store = &blockedServedStore{memStore: store, release: release}
	shadow, shadowRaw := asIn(t, ModeShadow)
	active, activeRaw := asIn(t, ModeActive)
	_, err := svc.Evaluate(context.Background(), shadow, shadowRaw, "s")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	_, err = svc.Evaluate(ctx, active, activeRaw, "a")
	require.NoError(t, err)
	cancel()       // the caller is gone before the background write goes on
	close(release) // and the write is let through after that
	waitServed(svc)

	assert.Len(t, store.snapshot(), 2)
}

// The counters carry the mode of the call; the alerts add the modes up.
func TestTheOracleMetricsCarryTheModeOfTheCall(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "Да."), good(VerdictVeto, "Не."), {err: context.DeadlineExceeded}}}
	svc, _, _ := newTestService(model)
	shadow, shadowRaw := asIn(t, ModeShadow)
	active, activeRaw := asIn(t, ModeActive)
	none, noneRaw := mustDecode(t, func(m map[string]any) {
		m["mode"] = ""
		m["signal"].(map[string]any)["signal_id"] = "sig-none"
	})
	other, otherRaw := mustDecode(t, func(m map[string]any) {
		m["mode"] = ModeActive
		m["signal"].(map[string]any)["signal_id"] = "sig-other"
	})

	answeredShadow, answeredActive := requestsIn(t, "answered", "shadow"), requestsIn(t, "answered", "active")
	cachedShadow, cachedActive := requestsIn(t, "cached", "shadow"), requestsIn(t, "cached", "active")
	approveShadow, approveActive := verdictsIn(t, "APPROVE", "shadow"), verdictsIn(t, "APPROVE", "active")
	vetoNone := verdictsIn(t, "VETO", "none")
	failedActive := requestsIn(t, "failed", "active")
	latencyShadow, latencyActive, latencyNone := latencyCountIn(t, "shadow"), latencyCountIn(t, "active"), latencyCountIn(t, "none")

	// shadow asks the model; active repeats it (cached, in the mode of the ASKER, not of the first);
	// shadow once more is a repeat in its own mode.
	_, err := svc.Evaluate(context.Background(), shadow, shadowRaw, "1")
	require.NoError(t, err)
	_, err = svc.Evaluate(context.Background(), active, activeRaw, "2")
	require.NoError(t, err)
	_, err = svc.Evaluate(context.Background(), shadow, shadowRaw, "3")
	require.NoError(t, err)
	// a call with no mode that the model answers
	_, err = svc.Evaluate(context.Background(), none, noneRaw, "4")
	require.NoError(t, err)
	// a call in active mode that the model cannot answer
	_, err = svc.Evaluate(context.Background(), other, otherRaw, "5")
	require.Error(t, err)
	waitServed(svc)

	assert.Equal(t, answeredShadow+1, requestsIn(t, "answered", "shadow"))
	assert.Equal(t, answeredActive, requestsIn(t, "answered", "active"), "active did not reach the model")
	assert.Equal(t, cachedActive+1, requestsIn(t, "cached", "active"))
	assert.Equal(t, cachedShadow+1, requestsIn(t, "cached", "shadow"))
	assert.Equal(t, approveShadow+1, verdictsIn(t, "APPROVE", "shadow"), "the verdict is the one of the call that made the model give it")
	assert.Equal(t, approveActive, verdictsIn(t, "APPROVE", "active"))
	assert.Equal(t, vetoNone+1, verdictsIn(t, "VETO", "none"))
	assert.Equal(t, failedActive+1, requestsIn(t, "failed", "active"))
	assert.Equal(t, latencyShadow+1, latencyCountIn(t, "shadow"))
	assert.Equal(t, latencyNone+1, latencyCountIn(t, "none"))
	assert.Equal(t, latencyActive+1, latencyCountIn(t, "active"), "the failed call in active mode reached the model too")
}
