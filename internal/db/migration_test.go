package db_test

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestMigrations(t *testing.T) {
	connString := os.Getenv("DATABASE_URL")
	if connString == "" {
		t.Skip("DATABASE_URL not set")
	}

	db, err := sql.Open("pgx", connString)
	require.NoError(t, err)
	defer db.Close()

	err = goose.SetDialect("postgres")
	require.NoError(t, err)

	// Run all migrations up
	err = goose.Up(db, "../../migrations")
	require.NoError(t, err)

	// Run all migrations down
	err = goose.DownTo(db, "../../migrations", 0)
	require.NoError(t, err)

	// Run back up to leave the DB in a usable state for other tests if they run later
	err = goose.Up(db, "../../migrations")
	require.NoError(t, err)
}
