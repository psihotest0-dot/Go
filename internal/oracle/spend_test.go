package oracle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// listStore serves rows newest first, the way the journal does.
type listStore struct{ rows []Evaluation }

func (s *listStore) Save(context.Context, Evaluation) (Evaluation, error) { return Evaluation{}, nil }
func (s *listStore) List(_ context.Context, f ListFilter) ([]Evaluation, error) {
	var out []Evaluation
	for _, r := range s.rows {
		if f.BeforeID == 0 || r.ID < f.BeforeID {
			out = append(out, r)
		}
		if len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

func TestSpentTodayCountsOnlyTodaysPaidRows(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.Local)
	today := now.Add(-time.Hour)
	yesterday := now.Add(-20 * time.Hour)
	store := &listStore{rows: []Evaluation{
		{ID: 5, CreatedAt: today, Model: "deepseek-flash", PromptTokens: 1000000, CompletionTokens: 0},
		{ID: 4, CreatedAt: today, Model: "Gemma-4-12B-it-GGUF", PromptTokens: 5000000, CompletionTokens: 5000000},
		{ID: 3, CreatedAt: today, Model: "deepseek-flash", PromptTokens: 0, CompletionTokens: 1000000},
		{ID: 2, CreatedAt: yesterday, Model: "deepseek-flash", PromptTokens: 9000000, CompletionTokens: 0},
	}}

	spent, err := SpentToday(context.Background(), store, now)

	require.NoError(t, err)
	assert.InDelta(t, 0.30+1.20, spent, 1e-9, "1M input as cache miss + 1M output; the local model and yesterday are not counted")
	assert.InDelta(t, llm.EstimateCost("deepseek-flash", 0, 1e6, 0)+llm.EstimateCost("deepseek-flash", 0, 0, 1e6), spent, 1e-9)
}

func TestSpentTodayOfAnEmptyJournalIsNothing(t *testing.T) {
	spent, err := SpentToday(context.Background(), &listStore{}, time.Now())

	require.NoError(t, err)
	assert.Zero(t, spent)
}

func TestSpentTodayReadsMoreThanOnePage(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.Local)
	store := &listStore{}
	for id := int64(spendPageSize*2 + 50); id >= 1; id-- {
		store.rows = append(store.rows, Evaluation{ID: id, CreatedAt: now.Add(-time.Minute), Model: "deepseek-flash", PromptTokens: 1000})
	}

	spent, err := SpentToday(context.Background(), store, now)

	require.NoError(t, err)
	assert.InDelta(t, float64(spendPageSize*2+50)*llm.EstimateCost("deepseek-flash", 0, 1000, 0), spent, 1e-9)
}
