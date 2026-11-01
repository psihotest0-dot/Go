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

	flag.CommandLine = flag.NewFlagSet("migrate", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"migrate"}, args...)
	return run()
}

// The migrations folder is relative to the working directory, so starting the
// command anywhere but the repository root has to say so instead of reporting a
// database that is "up to date".
func TestRefusesToRunOutsideTheRepositoryRoot(t *testing.T) {
	t.Chdir(t.TempDir())

	err := runWith(t, "-status")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "run this from the repository root")
}

func TestUnreachableDatabaseIsReported(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(dir+string(os.PathSeparator)+"migrations", 0o755))
	t.Chdir(dir)
	t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")

	require.Error(t, runWith(t, "-status"))
	require.Error(t, runWith(t))
}
