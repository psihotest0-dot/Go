package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// restrictViolation is the SQLSTATE migration 00018 raises with.
const restrictViolation = "23001"

// TestUserDeleteGuard covers migration 00018: a bare DELETE (or TRUNCATE) on
// users must fail, because every table with user data cascades from it
// (the 2026-10-30 incident), while a transaction that opts in
// explicitly still works and still cascades.
func TestUserDeleteGuard(t *testing.T) {
	connString := dbtest.ConnString(t)

	rawDB, err := sql.Open("pgx", connString)
	require.NoError(t, err)
	defer rawDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(rawDB, "../../migrations"))

	ctx := context.Background()
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	// Registered before the user cleanup below, so the pool is still open when
	// that cleanup runs (cleanups run last-in, first-out).
	t.Cleanup(database.Close)

	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	_, err = database.Pool.Exec(ctx,
		"INSERT INTO messages (user_id, session_id, role, content) VALUES ($1, 'guard-test', 'user', 'keep me')", user.ID)
	require.NoError(t, err)

	countMessages := func() int {
		var n int
		require.NoError(t, database.Pool.QueryRow(ctx,
			"SELECT count(*) FROM messages WHERE user_id = $1", user.ID).Scan(&n))
		return n
	}
	requireGuardError := func(err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, restrictViolation, pgErr.Code)
		assert.Contains(t, pgErr.Message, "app.allow_user_delete",
			"the error must tell the operator how to opt in")
	}

	t.Run("plain DELETE is refused", func(t *testing.T) {
		_, err := database.Pool.Exec(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		requireGuardError(err)
		assert.Equal(t, 1, countMessages(), "a refused delete must not cascade")
	})

	t.Run("DELETE without WHERE is refused", func(t *testing.T) {
		_, err := database.Pool.Exec(ctx, "DELETE FROM users")
		requireGuardError(err)
		assert.Equal(t, 1, countMessages())
	})

	t.Run("TRUNCATE is refused", func(t *testing.T) {
		_, err := database.Pool.Exec(ctx, "TRUNCATE users CASCADE")
		requireGuardError(err)
		assert.Equal(t, 1, countMessages())
	})

	t.Run("the opt-in does not outlive its transaction", func(t *testing.T) {
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, "SET LOCAL app.allow_user_delete = 'on'")
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))

		_, err = database.Pool.Exec(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		requireGuardError(err)
	})

	t.Run("explicit opt-in deletes the user and cascades", func(t *testing.T) {
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		_, err = tx.Exec(ctx, "SET LOCAL app.allow_user_delete = 'on'")
		require.NoError(t, err)
		tag, err := tx.Exec(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		assert.EqualValues(t, 1, tag.RowsAffected())
		require.NoError(t, tx.Commit(ctx))

		assert.Equal(t, 0, countMessages(), "the cascade still removes the user's rows")
	})
}
