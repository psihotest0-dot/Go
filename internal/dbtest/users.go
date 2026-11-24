package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TxBeginner is satisfied by *pgxpool.Pool.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DeleteUser removes one user created by a test, together with everything
// that cascades from it.
//
// Migration 00018 makes a plain `DELETE FROM users` fail, because that
// statement is what emptied the library on 2026-10-30 (see
// the 2026-10-30 incident report). The delete therefore runs in its own
// transaction with app.allow_user_delete switched on, and only for the single
// id the test owns. A failure is reported on t instead of being ignored, so a
// cleanup that stopped working cannot leave rows behind unnoticed.
func DeleteUser(t testing.TB, pool TxBeginner, id uuid.UUID) {
	t.Helper()

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Errorf("dbtest.DeleteUser: begin: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL app.allow_user_delete = 'on'"); err != nil {
		t.Errorf("dbtest.DeleteUser: allow delete: %v", err)
		return
	}
	if _, err := tx.Exec(ctx, "DELETE FROM users WHERE id = $1", id); err != nil {
		t.Errorf("dbtest.DeleteUser: delete user %s: %v", id, err)
		return
	}
	// The user took the last chunks of the table with it: the indexes are built again on the empty
	// table, so that the next test does not inherit an HNSW graph worn by the fill and empty of the
	// tests before it (see RebuildChunkIndexes). An empty table makes it a matter of milliseconds.
	var empty bool
	if err := tx.QueryRow(ctx, "SELECT NOT EXISTS (SELECT 1 FROM document_chunks)").Scan(&empty); err != nil {
		t.Errorf("dbtest.DeleteUser: count the chunks: %v", err)
		return
	}
	if empty {
		if _, err := tx.Exec(ctx, "REINDEX TABLE document_chunks"); err != nil {
			t.Errorf("dbtest.DeleteUser: rebuild the chunk indexes: %v", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("dbtest.DeleteUser: commit: %v", err)
	}
}
