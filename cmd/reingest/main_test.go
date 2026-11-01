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

	flag.CommandLine = flag.NewFlagSet("reingest", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"reingest"}, args...)
	return run()
}

func TestNothingToDoIsAnError(t *testing.T) {
	err := runWith(t)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to do")
}

func TestAllAndFileExcludeEachOther(t *testing.T) {
	err := runWith(t, "-all", "-file", "e420d62e-9388-4dc9-a262-1dcc6f714915")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exclude each other")
}

func TestFileIDsMustBeUUIDs(t *testing.T) {
	err := runWith(t, "-file", "e420d62e-9388-4dc9-a262-1dcc6f714915,книга")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a file id")
	assert.Contains(t, err.Error(), "книга")
}

// Nothing is queued unless the database answers: an unreachable one is an error,
// not a silent "done".
func TestUnreachableDatabaseIsReported(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")

	require.Error(t, runWith(t, "-list"))
	require.Error(t, runWith(t, "-all"))
}
