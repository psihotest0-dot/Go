package llm

import (
	"errors"
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

// SpendGuard counts what the paid model has cost today and refuses requests at the limit.
// The day is the local calendar day. The count lives in memory: a restart starts it again
// from what the caller seeds (the Oracle's journal knows what the day has cost so far).
type SpendGuard struct {
	limit float64
	now   func() time.Time

	mu    sync.Mutex
	day   string
	spent float64
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

// Add counts dollars spent.
func (g *SpendGuard) Add(usd float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollLocked()
	g.spent += usd
	metrics.PaidLLMSpendToday.Set(g.spent)
}

// Spent is what the day has cost so far, at the peak prices.
func (g *SpendGuard) Spent() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollLocked()
	return g.spent
}
