package db_test

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// scratchDatabase creates an empty database next to the test one and drops it
// afterwards. Its name contains "test", like dbtest requires of every database a
// test may change; the shared test database is not touched, because other
// packages use it and migrating it backwards would break them.
func scratchDatabase(t *testing.T) string {
	t.Helper()

	adminURL := dbtest.ConnString(t)
	admin, err := sql.Open("pgx", adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	name := "app_test_pending_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	_, err = admin.ExecContext(context.Background(), "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		assert.NoError(t, dropErr)
	})

	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	u.Path = "/" + name
	require.Contains(t, dbtest.DatabaseName(u.String()), "test")
	return u.String()
}

func TestPendingMigrationsFollowsTheDatabase(t *testing.T) {
	conn := scratchDatabase(t)
	// db.MigrationsDir is relative to the working directory (the repository root).
	t.Chdir("../..")

	files, err := filepath.Glob(filepath.Join(db.MigrationsDir, "*.sql"))
	require.NoError(t, err)
	require.Greater(t, len(files), 6)

	// A migration's version is the number in its file name; the numbering has gaps,
	// so the version is not the count of files.
	versionOf := func(file string) int64 {
		n, convErr := strconv.ParseInt(strings.SplitN(filepath.Base(file), "_", 2)[0], 10, 64)
		require.NoError(t, convErr, file)
		return n
	}
	const appliedUpTo = 5
	var olderThanCut, newerThanCut []string
	for _, file := range files {
		if versionOf(file) <= appliedUpTo {
			olderThanCut = append(olderThanCut, filepath.Base(file))
		} else {
			newerThanCut = append(newerThanCut, filepath.Base(file))
		}
	}
	require.NotEmpty(t, olderThanCut)
	require.NotEmpty(t, newerThanCut)

	// A database nobody has migrated is behind by every file.
	status, err := db.PendingMigrations(conn)
	require.NoError(t, err)
	assert.Zero(t, status.Current)
	require.Len(t, status.Pending, len(files))
	assert.Equal(t, filepath.Base(files[0]), status.Pending[0])

	// Apply the migrations up to version 5: the rest are pending, in order.
	sqlDB, err := sql.Open("pgx", conn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpTo(sqlDB, db.MigrationsDir, appliedUpTo))

	status, err = db.PendingMigrations(conn)
	require.NoError(t, err)
	assert.Equal(t, versionOf(olderThanCut[len(olderThanCut)-1]), status.Current)
	assert.Equal(t, newerThanCut, status.Pending)

	// Looking is not changing: asking again gives the same answer.
	again, err := db.PendingMigrations(conn)
	require.NoError(t, err)
	assert.Equal(t, status, again)

	// Applying the rest leaves nothing pending.
	require.NoError(t, db.RunMigrations(conn))
	status, err = db.PendingMigrations(conn)
	require.NoError(t, err)
	assert.Empty(t, status.Pending)
	assert.Equal(t, versionOf(files[len(files)-1]), status.Current)
}

func TestPendingMigrationsWithoutTheFolderIsAnError(t *testing.T) {
	conn := scratchDatabase(t)
	t.Chdir(t.TempDir()) // no migrations folder here

	_, err := db.PendingMigrations(conn)

	require.Error(t, err)
}
