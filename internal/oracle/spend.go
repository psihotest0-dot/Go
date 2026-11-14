package oracle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go-monolith/internal/llm"
)

const (
	spendPageSize = 200
	// spendMaxPages bounds the walk through the journal: 10 000 rows in one day is already a fault.
	spendMaxPages = 50
)

// SpentToday estimates what the paid model has cost the Oracle since the local midnight of
// now, from the journal, so that a restart does not give the daily limit a fresh start. The
// rows carry the tokens but not how many of the input came from the provider's cache, so
// all of it is priced as cache misses: an upper bound, which is what a limit wants.
func SpentToday(ctx context.Context, store Store, now time.Time) (float64, error) {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var spent float64
	var before int64
	for page := 0; page < spendMaxPages; page++ {
		rows, err := store.List(ctx, ListFilter{BeforeID: before, Limit: spendPageSize})
		if err != nil {
			return 0, fmt.Errorf("read the journal for today's cost: %w", err)
		}
		for _, row := range rows {
			if row.CreatedAt.Before(midnight) {
				return spent, nil
			}
			if strings.HasPrefix(row.Model, "deepseek") {
				spent += llm.EstimateCost(row.Model, 0, float64(row.PromptTokens), float64(row.CompletionTokens))
			}
			before = row.ID
		}
		if len(rows) < spendPageSize {
			break
		}
	}
	return spent, nil
}
