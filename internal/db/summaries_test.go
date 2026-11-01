package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// TestSessionSummaryQueries covers migration 00019 and its queries: the
// summary of a session, the messages still waiting to be summarised, and the
// rule that the covered position only ever moves forward.
func TestSessionSummaryQueries(t *testing.T) {
	connString := dbtest.ConnString(t)

	rawDB, err := sql.Open("pgx", connString)
	require.NoError(t, err)
	defer rawDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(rawDB, "../../migrations"))

	ctx := context.Background()
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })
	other, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }

	ids := make([]uuid.UUID, 10)
	for i := range ids {
		require.NoError(t, database.Pool.QueryRow(ctx,
			`INSERT INTO messages (user_id, session_id, role, content, created_at)
			 VALUES ($1, 'sess', 'user', $2, $3) RETURNING id`,
			user.ID, fmt.Sprintf("m%d", i), at(i)).Scan(&ids[i]))
	}
	// Same session id, another owner: never part of the user's results.
	_, err = database.Pool.Exec(ctx,
		`INSERT INTO messages (user_id, session_id, role, content, created_at) VALUES ($1, 'sess', 'user', 'foreign', $2)`,
		other.ID, at(5))
	require.NoError(t, err)

	pending := func(after, before time.Time, limit int32) []string {
		t.Helper()
		rows, err := database.Queries.GetMessagesToSummarize(ctx, db.GetMessagesToSummarizeParams{
			UserID: user.ID, SessionID: "sess", After: after, Before: before, Limit: limit,
		})
		require.NoError(t, err)
		contents := make([]string, len(rows))
		for i, r := range rows {
			contents[i] = r.Content
		}
		return contents
	}

	t.Run("pending messages lie strictly between the covered position and the prompt", func(t *testing.T) {
		assert.Equal(t, []string{"m3", "m4", "m5", "m6"}, pending(at(2), at(7), 100),
			"after is exclusive (covered), before is exclusive (in the prompt), ordered oldest first, other users left out")
		assert.Equal(t, []string{"m0", "m1"}, pending(time.Time{}, at(2), 100), "the zero time covers nothing")
		assert.Empty(t, pending(at(9), at(10), 100))
		assert.Empty(t, pending(at(5), at(5), 100))
	})

	t.Run("a limit keeps the oldest messages", func(t *testing.T) {
		assert.Equal(t, []string{"m0", "m1", "m2"}, pending(time.Time{}, at(100), 3))
	})

	t.Run("no summary yet", func(t *testing.T) {
		_, err := database.Queries.GetSessionSummary(ctx, db.GetSessionSummaryParams{UserID: user.ID, SessionID: "sess"})
		assert.ErrorIs(t, err, sql.ErrNoRows)
	})

	save := func(text string, i int) {
		t.Helper()
		require.NoError(t, database.Queries.UpsertSessionSummary(ctx, db.UpsertSessionSummaryParams{
			UserID: user.ID, SessionID: "sess", Summary: text, CoveredUntilMessageID: ids[i], CoveredUntilAt: at(i),
		}))
	}
	read := func() db.SessionSummary {
		t.Helper()
		row, err := database.Queries.GetSessionSummary(ctx, db.GetSessionSummaryParams{UserID: user.ID, SessionID: "sess"})
		require.NoError(t, err)
		return row
	}

	t.Run("the position only moves forward", func(t *testing.T) {
		save("first", 3)
		assert.Equal(t, "first", read().Summary)

		save("second", 6)
		row := read()
		assert.Equal(t, "second", row.Summary)
		assert.Equal(t, ids[6], row.CoveredUntilMessageID)
		assert.True(t, at(6).Equal(row.CoveredUntilAt))

		save("late", 4) // a slower pass that was overtaken
		row = read()
		assert.Equal(t, "second", row.Summary, "an older position must not overwrite a newer one")
		assert.Equal(t, ids[6], row.CoveredUntilMessageID)
	})

	t.Run("the summary belongs to its owner", func(t *testing.T) {
		_, err := database.Queries.GetSessionSummary(ctx, db.GetSessionSummaryParams{UserID: other.ID, SessionID: "sess"})
		assert.ErrorIs(t, err, sql.ErrNoRows)
	})

	t.Run("a regenerated answer does not break the position", func(t *testing.T) {
		// The position is a time, not a reference to a message, so deleting the
		// message it was taken from leaves it intact.
		_, err := database.Pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, ids[6])
		require.NoError(t, err)

		assert.True(t, at(6).Equal(read().CoveredUntilAt))
		assert.Equal(t, []string{"m7", "m8"}, pending(at(6), at(9), 100))
	})

	t.Run("deleting the user removes the summary", func(t *testing.T) {
		third, err := database.Queries.CreateUser(ctx)
		require.NoError(t, err)
		require.NoError(t, database.Queries.UpsertSessionSummary(ctx, db.UpsertSessionSummaryParams{
			UserID: third.ID, SessionID: "gone", Summary: "x", CoveredUntilMessageID: uuid.New(), CoveredUntilAt: at(1),
		}))

		dbtest.DeleteUser(t, database.Pool, third.ID)

		_, err = database.Queries.GetSessionSummary(ctx, db.GetSessionSummaryParams{UserID: third.ID, SessionID: "gone"})
		assert.ErrorIs(t, err, sql.ErrNoRows)
	})
}
