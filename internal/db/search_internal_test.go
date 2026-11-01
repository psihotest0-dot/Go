package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/dbtest"
)

// hnsw.ef_search must be in force inside the search transaction and gone outside
// it, so it cannot leak into other users of the pooled connection.
func TestSearchSettingsAreScopedToTheTransaction(t *testing.T) {
	ctx := context.Background()
	d, err := Connect(ctx, dbtest.ConnString(t))
	require.NoError(t, err)
	defer d.Close()

	show := func(tx *sql.Tx) string {
		var v string
		require.NoError(t, tx.QueryRowContext(ctx, `SHOW hnsw.ef_search`).Scan(&v))
		return v
	}

	for i := 0; i < 3; i++ { // the pool may hand back the same connection each time
		var inside string
		require.NoError(t, d.inSearchTx(ctx, func(tx *sql.Tx) error {
			inside = show(tx)
			return nil
		}))
		assert.Equal(t, fmt.Sprint(SearchEfSearch), inside)

		tx, err := d.SqlDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		// pgvector's default is 40, or an empty placeholder while its library is not
		// yet loaded in this session; what matters is that it is not our value.
		assert.NotEqual(t, fmt.Sprint(SearchEfSearch), show(tx), "the setting must not outlive the transaction")
		require.NoError(t, tx.Rollback())
	}
}

func TestSearchTransactionIsReadOnly(t *testing.T) {
	ctx := context.Background()
	d, err := Connect(ctx, dbtest.ConnString(t))
	require.NoError(t, err)
	defer d.Close()

	err = d.inSearchTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE should_not_exist (x int)`)
		return err
	})
	require.Error(t, err, "a search must not be able to write")
}
