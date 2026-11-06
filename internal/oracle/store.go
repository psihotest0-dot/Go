package oracle

import (
	"context"
	"database/sql"
	"fmt"

	"go-monolith/internal/db"
)

// Store is the journal of the Oracle.
type Store interface {
	// Save writes one row and returns it with its id and time.
	Save(ctx context.Context, e Evaluation) (Evaluation, error)
	// List returns the newest rows first, at most f.Limit of them.
	List(ctx context.Context, f ListFilter) ([]Evaluation, error)
}

// ListFilter selects a page of the journal.
type ListFilter struct {
	Symbol   string // empty: every symbol
	BeforeID int64  // 0: from the newest; else only rows with a smaller id
	Limit    int
}

// DBStore keeps the journal in the oracle_evaluations table.
type DBStore struct {
	q *db.Queries
}

// NewDBStore returns the journal stored in PostgreSQL.
func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) Save(ctx context.Context, e Evaluation) (Evaluation, error) {
	request := e.Request
	if len(request) == 0 {
		request = []byte("{}")
	}
	row, err := s.q.CreateOracleEvaluation(ctx, db.CreateOracleEvaluationParams{
		RequestID:        e.RequestID,
		SignalID:         e.SignalID,
		Symbol:           e.Symbol,
		Mode:             e.Mode,
		Status:           e.Status,
		Verdict:          sql.NullString{String: e.Verdict, Valid: e.Verdict != ""},
		Reason:           e.Reason,
		Model:            e.Model,
		Attempts:         int16(e.Attempts),
		FinishReason:     e.FinishReason,
		PromptTokens:     int32(e.PromptTokens),
		CompletionTokens: int32(e.CompletionTokens),
		LatencyMs:        int32(e.LatencyMS),
		Error:            e.Error,
		Request:          request,
		PromptVersion:    e.PromptVersion,
	})
	if err != nil {
		return e, fmt.Errorf("save the oracle evaluation: %w", err)
	}
	e.ID = row.ID
	e.CreatedAt = row.CreatedAt
	return e, nil
}

func (s *DBStore) List(ctx context.Context, f ListFilter) ([]Evaluation, error) {
	rows, err := s.q.ListOracleEvaluations(ctx, db.ListOracleEvaluationsParams{
		Symbol:   f.Symbol,
		BeforeID: f.BeforeID,
		RowLimit: int32(f.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list the oracle evaluations: %w", err)
	}
	out := make([]Evaluation, 0, len(rows))
	for _, r := range rows {
		out = append(out, Evaluation{
			ID:               r.ID,
			CreatedAt:        r.CreatedAt,
			RequestID:        r.RequestID,
			SignalID:         r.SignalID,
			Symbol:           r.Symbol,
			Mode:             r.Mode,
			Status:           r.Status,
			Verdict:          r.Verdict.String,
			Reason:           r.Reason,
			Model:            r.Model,
			Attempts:         int(r.Attempts),
			FinishReason:     r.FinishReason,
			PromptTokens:     int(r.PromptTokens),
			CompletionTokens: int(r.CompletionTokens),
			LatencyMS:        int(r.LatencyMs),
			Error:            r.Error,
			PromptVersion:    r.PromptVersion,
			SignalVersion:    r.SignalVersion.String,
		})
	}
	return out, nil
}
