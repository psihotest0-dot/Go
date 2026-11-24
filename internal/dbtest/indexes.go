package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is satisfied by *pgxpool.Pool.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RebuildChunkIndexes builds the indexes of document_chunks again, the HNSW ones among them. A test
// that needs the vector half of a search to find its passages calls it after it has put them in.
//
// Why (2026-11-24): the test database is filled and emptied all the time - every test puts its
// chunks in and deletes its user at the end - and after enough of that, with autovacuum between,
// the HNSW graph of a partial index can be left with no way from its entry point to a live row: the
// index scan of the Cyrillic chunks returned 0 rows in 0.04 ms while the table held two of them (a
// plain scan found both), and TestSearchDocumentsMergesWhatTheEnglishQueryFound failed in about one
// run of the agent package in three. The live library, which is filled once and rarely loses a
// file, is not in this state. The table of a test holds a few rows, so the rebuild takes
// milliseconds.
//
// It refuses any database without "test" in its name, as ConnString does.
func RebuildChunkIndexes(t testing.TB, db Execer) {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := db.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("dbtest.RebuildChunkIndexes: %v", err)
	}
	if !strings.Contains(strings.ToLower(name), "test") {
		t.Fatalf("dbtest.RebuildChunkIndexes: %q is not a test database", name)
	}
	if _, err := db.Exec(ctx, "REINDEX TABLE document_chunks"); err != nil {
		t.Fatalf("dbtest.RebuildChunkIndexes: %v", err)
	}
}
