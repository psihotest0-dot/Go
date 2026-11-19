package db

import (
	"context"
	"database/sql"
	"errors"
)

// PaidSpendStore keeps the daily cost of the paid model in the database. It satisfies
// llm.SpendStore (the llm package does not import this one).
type PaidSpendStore struct {
	q *Queries
}

// NewPaidSpendStore returns the store over the queries.
func NewPaidSpendStore(q *Queries) *PaidSpendStore { return &PaidSpendStore{q: q} }

// SpentOn is what the day ("YYYY-MM-DD") has cost so far: zero when nothing was stored.
func (s *PaidSpendStore) SpentOn(ctx context.Context, day string) (float64, error) {
	usd, err := s.q.GetPaidLLMSpend(ctx, day)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return usd, err
}

// AddSpent adds dollars to the day.
func (s *PaidSpendStore) AddSpent(ctx context.Context, day string, usd float64) error {
	return s.q.AddPaidLLMSpend(ctx, AddPaidLLMSpendParams{Day: day, Usd: usd})
}
