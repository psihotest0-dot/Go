package llm

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go-monolith/metrics"
)

// ErrSpendCap is returned, without any request, when the paid model has already cost its
// daily limit (PAID_LLM_DAILY_USD). It is not retried: the next day is the only help.
var ErrSpendCap = errors.New("the daily limit of the paid LLM is reached")

// price is dollars per million tokens: input from the provider's cache, input that was not
// in it, and output.
type price struct{ hit, miss, out float64 }

// prices are the PEAK prices of api-docs.deepseek.com/quick_start/pricing on 2026-11-14. Off
// peak the provider charges half, so the estimate is an upper bound, which is what a limit
// wants. A model that is not here is priced as the dearest one. Update the table when the
// provider changes its prices (the README says where they are).
var prices = map[string]price{
	"deepseek-flash":  {hit: 0.006, miss: 0.30, out: 1.20},
	"deepseek-v4-pro": {hit: 0.044, miss: 1.32, out: 3.96},
}

// EstimateCost is the dollars that the tokens cost at the peak prices of the model.
func EstimateCost(model string, hit, miss, out float64) float64 {
	p, ok := prices[model]
	if !ok {
		p = prices["deepseek-v4-pro"]
	}
	return (hit*p.hit + miss*p.miss + out*p.out) / 1e6
}

// SpendStore keeps what the paid model has cost, per day, so that a restart does not give
// the daily limit a fresh start. The day is "YYYY-MM-DD", the local date of the process.
type SpendStore interface {
	// SpentOn is what the day has cost so far (zero when there is no row).
	SpentOn(ctx context.Context, day string) (float64, error)
	// AddSpent adds dollars to the day.
	AddSpent(ctx context.Context, day string, usd float64) error
}

// spendStoreTimeout bounds one write of the cost of a request.
const spendStoreTimeout = 3 * time.Second

// SpendGuard counts what the paid model has cost today and refuses requests at the limit.
// The day is the local calendar day. The count is kept in memory and, once Resume has
// been called, also in a SpendStore (a database table): a restart goes on from what the
// store and the Oracle's journal know.
type SpendGuard struct {
	limit float64
	now   func() time.Time

	mu    sync.Mutex
	day   string
	spent float64
	store SpendStore
}

// NewSpendGuard returns a guard with the limit in dollars per day.
func NewSpendGuard(limitUSD float64) *SpendGuard {
	g := &SpendGuard{limit: limitUSD, now: time.Now}
	metrics.PaidLLMSpendLimit.Set(limitUSD)
	return g
}

// rollLocked starts a new day when the date has changed. The caller holds the lock.
func (g *SpendGuard) rollLocked() {
	if today := g.now().Format("2006-01-02"); today != g.day {
		g.day, g.spent = today, 0
		metrics.PaidLLMSpendToday.Set(0)
	}
}

// Allow is nil while the limit is not reached.
func (g *SpendGuard) Allow() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollLocked()
	if g.spent >= g.limit {
		metrics.PaidLLMRequests.WithLabelValues(deepSeekProvider, "limit").Inc()
		return ErrSpendCap
	}
	return nil
}

// Add counts dollars spent, and writes them to the store when there is one. A failed
// write is logged and the request goes on: the count in memory is right, and the limit
// is a safety net, not an account book.
func (g *SpendGuard) Add(usd float64) {
	g.mu.Lock()
	g.rollLocked()
	g.spent += usd
	metrics.PaidLLMSpendToday.Set(g.spent)
	store, day := g.store, g.day
	g.mu.Unlock()

	if store == nil || usd <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), spendStoreTimeout)
	defer cancel()
	if err := store.AddSpent(ctx, day, usd); err != nil {
		slog.Warn("could not store what the paid model cost; a restart may forget it", slog.Any("error", err), slog.Float64("usd", usd))
	}
}

// Resume makes the count survive restarts. It starts today from the larger of what the
// store holds and journalUSD (an estimate from the Oracle's journal, which is all the
// store knew before it existed), writes the difference to the store so that the two agree,
// and from then on every Add is stored too. Call it once at start, before any request.
func (g *SpendGuard) Resume(ctx context.Context, store SpendStore, journalUSD float64) {
	g.mu.Lock()
	g.rollLocked()
	day := g.day
	g.mu.Unlock()

	saved, err := store.SpentOn(ctx, day)
	if err != nil {
		slog.Warn("could not read what the paid model cost today from the store; counting from the journal", slog.Any("error", err))
		saved = 0
	}
	start := max(saved, journalUSD)
	if gap := start - saved; err == nil && gap > 0 {
		if err := store.AddSpent(ctx, day, gap); err != nil {
			slog.Warn("could not store what the paid model cost today", slog.Any("error", err))
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollLocked()
	g.spent = max(g.spent, start)
	g.store = store
	metrics.PaidLLMSpendToday.Set(g.spent)
}

// Spent is what the day has cost so far, at the peak prices.
func (g *SpendGuard) Spent() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollLocked()
	return g.spent
}
