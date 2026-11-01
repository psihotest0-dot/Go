package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// Settings of the similarity searches. They are starting values: the cosine
// distance thresholds are re-measured against the real library in task 32 of the
// project plan.
const (
	// SearchVectorCandidates is how many nearest chunks the HNSW scan returns
	// before the user, status and type filters are applied.
	SearchVectorCandidates = 200

	// SearchEfSearch is hnsw.ef_search. The index returns at most this many rows
	// (default 40), so it must not be smaller than SearchVectorCandidates.
	SearchEfSearch = SearchVectorCandidates

	// SearchMinTextSimilarity is the word_similarity a memory needs to count as a
	// text match (the memories table is small and has no text index; chunks use
	// full-text search).
	SearchMinTextSimilarity = 0.35
)

// WithSearchSettings runs fn in a read-only transaction in which the settings
// the similarity searches rely on are in force (hnsw.ef_search). The setting is
// per transaction (set_config with is_local), so it cannot leak into other users
// of the pooled connection.
func (d *Database) WithSearchSettings(ctx context.Context, fn func(q *Queries) error) error {
	return d.inSearchTx(ctx, func(tx *sql.Tx) error { return fn(d.Queries.WithTx(tx)) })
}

func (d *Database) inSearchTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.SqlDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("failed to begin the search transaction: %w", err)
	}
	// A read-only transaction has nothing to commit; Rollback ends it either way.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SELECT set_config('hnsw.ef_search', $1, true)", strconv.Itoa(SearchEfSearch)); err != nil {
		return fmt.Errorf("failed to set hnsw.ef_search: %w", err)
	}
	return fn(tx)
}
