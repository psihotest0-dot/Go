package main

import (
	"flag"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runWith runs the command as if it was started with args. run reads the global
// flag set, so each call gets a fresh one (and the real one is put back after).
func runWith(t *testing.T, args ...string) error {
	t.Helper()

	savedArgs, savedFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = savedArgs, savedFlags })

	flag.CommandLine = flag.NewFlagSet("reembed", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"reembed"}, args...)
	return run()
}

// No vector is touched unless the database answers.
func TestUnreachableDatabaseIsReported(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")

	err := runWith(t, "-force")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to database")
}

// -migrate runs the migrations first, and a failure there stops the command
// before any vector is rewritten.
func TestMigrateFlagStopsOnAnUnreachableDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")

	require.Error(t, runWith(t, "-migrate"))
}
