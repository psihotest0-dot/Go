package dbtest

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// fakeDB answers current_database() with name and records the statements it was given.
type fakeDB struct {
	name  string
	execs []string
}

type nameRow struct{ name string }

func (r nameRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.name
	return nil
}

func (d *fakeDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	d.execs = append(d.execs, sql)
	return pgconn.CommandTag{}, nil
}

func (d *fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return nameRow{d.name} }

func rebuild(db *fakeDB) *fakeTB {
	tb := &fakeTB{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		RebuildChunkIndexes(tb, db)
	}()
	wg.Wait()
	return tb
}

func TestTheChunkIndexesAreRebuiltOnlyInATestDatabase(t *testing.T) {
	test := &fakeDB{name: "app_test"}
	assert.Empty(t, rebuild(test).fatal)
	assert.Equal(t, []string{"REINDEX TABLE document_chunks"}, test.execs)

	live := &fakeDB{name: "postgres"}
	assert.Contains(t, rebuild(live).fatal, "is not a test database")
	assert.Empty(t, live.execs, "nothing is run on the live database")
}
