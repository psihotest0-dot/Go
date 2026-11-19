package llm

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/metrics"
)

func TestEstimateCostUsesThePeakPricesOfTheModel(t *testing.T) {
	// 1M tokens of each kind.
	assert.InDelta(t, 0.006+0.30+1.20, EstimateCost("deepseek-flash", 1e6, 1e6, 1e6), 1e-9)
	assert.InDelta(t, 0.044+1.32+3.96, EstimateCost("deepseek-v4-pro", 1e6, 1e6, 1e6), 1e-9)
	assert.InDelta(t, 1.32, EstimateCost("deepseek-v4-pro", 0, 1e6, 0), 1e-9)
	assert.InDelta(t, 0.0, EstimateCost("deepseek-flash", 0, 0, 0), 1e-12)
}

func TestAnUnknownModelIsPricedAsTheDearestOne(t *testing.T) {
	assert.Equal(t, EstimateCost("deepseek-v4-pro", 10, 20, 30), EstimateCost("deepseek-vNext", 10, 20, 30),
		"a limit that guesses low is no limit")
}

func TestTheGuardRefusesAtTheLimit(t *testing.T) {
	g := NewSpendGuard(1.0)

	require.NoError(t, g.Allow())
	g.Add(0.99)
	require.NoError(t, g.Allow(), "just under the limit")
	g.Add(0.01)

	assert.ErrorIs(t, g.Allow(), ErrSpendCap)
	assert.InDelta(t, 1.0, g.Spent(), 1e-9)
}

func TestTheGuardStartsANewDay(t *testing.T) {
	now := time.Date(2026, 11, 14, 23, 59, 0, 0, time.Local)
	g := NewSpendGuard(1.0)
	g.now = func() time.Time { return now }
	g.Add(2.0)
	require.ErrorIs(t, g.Allow(), ErrSpendCap)

	now = now.Add(2 * time.Minute) // past midnight

	require.NoError(t, g.Allow(), "tomorrow the Oracle may ask again")
	assert.Zero(t, g.Spent())
}

func TestTheGaugesFollowTheGuard(t *testing.T) {
	g := NewSpendGuard(2.5)
	g.Add(0.25)

	assert.InDelta(t, 2.5, gaugeValue(t, metrics.PaidLLMSpendLimit), 1e-9)
	assert.InDelta(t, 0.25, gaugeValue(t, metrics.PaidLLMSpendToday), 1e-9)
}

type wrapErr struct{ err error }

func (w wrapErr) Error() string { return "wrapped: " + w.err.Error() }
func (w wrapErr) Unwrap() error { return w.err }

func TestACappedErrorIsNotRetried(t *testing.T) {
	assert.False(t, retryable(context.Background(), ErrSpendCap))
	assert.False(t, retryable(context.Background(), wrapErr{ErrSpendCap}))
}

// At the limit nothing leaves the machine; below it the cost of every reply is added.
func TestThePaidClientStopsAtTheLimitAndCountsWhatItSpends(t *testing.T) {
	var calls int
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, http.StatusOK, deepSeekReply) // 300 hit, 700 miss, 50 out
	}
	guard := NewSpendGuard(0.001)
	c := newTestClient(t, Options{Dialect: DialectDeepSeek, APIKey: "k", Model: "deepseek-flash", ForceModel: true, Spend: guard}, handler)

	_, err := c.Chat(context.Background(), userRequest("one"))
	require.NoError(t, err)
	// 300*0.006 + 700*0.30 + 50*1.20 = 1.8 + 210 + 60 = 271.8 per million -> $0.0002718
	assert.InDelta(t, 0.0002718, guard.Spent(), 1e-9)

	for i := 0; i < 3; i++ {
		_, err = c.Chat(context.Background(), userRequest("more"))
		require.NoError(t, err)
	}
	require.GreaterOrEqual(t, guard.Spent(), 0.001)
	callsAtTheLimit := calls

	_, err = c.Chat(context.Background(), userRequest("too much"))

	require.ErrorIs(t, err, ErrSpendCap)
	assert.Equal(t, callsAtTheLimit, calls, "the request that would cross the limit is not even sent")
}

// memStore is a SpendStore in memory.
type memStore struct {
	mu      sync.Mutex
	days    map[string]float64
	readErr error
	addErr  error
}

func newMemStore() *memStore { return &memStore{days: map[string]float64{}} }

func (s *memStore) SpentOn(_ context.Context, day string) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.days[day], s.readErr
}

func (s *memStore) AddSpent(_ context.Context, day string, usd float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addErr != nil {
		return s.addErr
	}
	s.days[day] += usd
	return nil
}

func guardOn(day time.Time, limit float64) (*SpendGuard, *time.Time) {
	now := day
	g := NewSpendGuard(limit)
	g.now = func() time.Time { return now }
	return g, &now
}

// The point of the table: what the chat spent is not forgotten by a restart.
func TestTheCostSurvivesARestart(t *testing.T) {
	day := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)
	store := newMemStore()

	first, _ := guardOn(day, 1.0)
	first.Resume(context.Background(), store, 0)
	first.Add(0.4)
	first.Add(0.3)
	assert.InDelta(t, 0.7, store.days["2026-11-19"], 1e-9, "every request is stored")

	second, _ := guardOn(day, 1.0) // the process restarted
	second.Resume(context.Background(), store, 0)

	assert.InDelta(t, 0.7, second.Spent(), 1e-9, "the new process goes on from the stored cost")
	second.Add(0.3)
	assert.ErrorIs(t, second.Allow(), ErrSpendCap, "so the limit is reached where it would have been")
}

// The journal of the Oracle knows what the day cost before the table existed: the larger of
// the two wins, and the table is brought up to it.
func TestResumeStartsFromTheLargerOfTheStoreAndTheJournal(t *testing.T) {
	day := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)

	store := newMemStore()
	g, _ := guardOn(day, 5)
	g.Resume(context.Background(), store, 0.25)
	assert.InDelta(t, 0.25, g.Spent(), 1e-9)
	assert.InDelta(t, 0.25, store.days["2026-11-19"], 1e-9, "the table catches up with the journal")

	store = newMemStore()
	store.days["2026-11-19"] = 0.9
	g, _ = guardOn(day, 5)
	g.Resume(context.Background(), store, 0.25)
	assert.InDelta(t, 0.9, g.Spent(), 1e-9)
	assert.InDelta(t, 0.9, store.days["2026-11-19"], 1e-9, "a smaller journal does not lower it")
}

func TestTheStoreFollowsTheDay(t *testing.T) {
	store := newMemStore()
	g, now := guardOn(time.Date(2026, 11, 19, 23, 59, 0, 0, time.Local), 5)
	g.Resume(context.Background(), store, 0)
	g.Add(0.5)

	*now = now.Add(2 * time.Minute)
	g.Add(0.2)

	assert.InDelta(t, 0.5, store.days["2026-11-19"], 1e-9)
	assert.InDelta(t, 0.2, store.days["2026-11-20"], 1e-9, "past midnight the cost goes to the new day")
	assert.InDelta(t, 0.2, g.Spent(), 1e-9)
}

// A store that fails must not stop the chat or lose the count in memory.
func TestAFailingStoreDoesNotStopTheGuard(t *testing.T) {
	day := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)
	store := newMemStore()
	store.readErr = errors.New("database is down")
	g, _ := guardOn(day, 1.0)

	g.Resume(context.Background(), store, 0.1)
	assert.InDelta(t, 0.1, g.Spent(), 1e-9, "counted from the journal when the store cannot be read")
	assert.Empty(t, store.days, "nothing is written on top of a table that could not be read")

	store.addErr = errors.New("database is down")
	g.Add(0.2)
	assert.InDelta(t, 0.3, g.Spent(), 1e-9, "the count in memory is right")
	require.NoError(t, g.Allow())
}

func TestNothingIsStoredForAFreeRequest(t *testing.T) {
	store := newMemStore()
	g, _ := guardOn(time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local), 1.0)
	g.Resume(context.Background(), store, 0)

	g.Add(0)

	assert.Empty(t, store.days)
}
