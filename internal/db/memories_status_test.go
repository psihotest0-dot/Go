package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/dbtest"
)

// A replaced memory is deleted (migration 00029), so the database no longer takes
// "superseded" as a status; candidate and active stay.
func TestMemoryStatusIsCandidateOrActive(t *testing.T) {
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, RunMigrations(connString))
	ctx := context.Background()
	d, err := Connect(ctx, connString)
	require.NoError(t, err)
	defer d.Close()
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	defer dbtest.DeleteUser(t, d.Pool, user.ID)

	insert := func(status string) error {
		_, err := d.SqlDB.ExecContext(ctx,
			`INSERT INTO memories (user_id, content, status, embedding_model, importance, content_hash)
			 VALUES ($1, $2, $2, 'x', 1, $2)`, user.ID, status)
		return err
	}
	assert.NoError(t, insert("candidate"))
	assert.NoError(t, insert("active"))
	assert.ErrorContains(t, insert("superseded"), "memories_status_check")
}
