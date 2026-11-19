package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/dbtest"
)

// The daily cost of the paid model in the real table: adding accumulates, a day nobody
// paid on reads as zero, and the table takes only a date as its key.
func TestPaidSpendStoreAccumulatesPerDay(t *testing.T) {
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, RunMigrations(connString))
	ctx := context.Background()
	d, err := Connect(ctx, connString)
	require.NoError(t, err)
	defer d.Close()
	store := NewPaidSpendStore(d.Queries)
	const day, other = "2099-01-01", "2099-01-02"
	_, err = d.SqlDB.ExecContext(ctx, `DELETE FROM paid_llm_spend WHERE day IN ($1, $2)`, day, other)
	require.NoError(t, err)
	defer d.SqlDB.ExecContext(ctx, `DELETE FROM paid_llm_spend WHERE day IN ($1, $2)`, day, other) //nolint:errcheck

	got, err := store.SpentOn(ctx, day)
	require.NoError(t, err)
	assert.Zero(t, got, "no row is zero, not an error")

	require.NoError(t, store.AddSpent(ctx, day, 0.25))
	require.NoError(t, store.AddSpent(ctx, day, 0.5))
	require.NoError(t, store.AddSpent(ctx, other, 0.125))

	got, err = store.SpentOn(ctx, day)
	require.NoError(t, err)
	assert.InDelta(t, 0.75, got, 1e-9)
	got, err = store.SpentOn(ctx, other)
	require.NoError(t, err)
	assert.InDelta(t, 0.125, got, 1e-9, "days do not mix")

	assert.Error(t, store.AddSpent(ctx, "tomorrow", 0.1), "the key is a date")
	assert.Error(t, store.AddSpent(ctx, day, -1), "a cost cannot be negative")
}
