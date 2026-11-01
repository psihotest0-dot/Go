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

	flag.CommandLine = flag.NewFlagSet("recategorize", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"recategorize"}, args...)
	return run()
}

// Nothing is read or written unless the database answers.
func TestUnreachableDatabaseIsReported(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/app?sslmode=disable&connect_timeout=2")

	for _, args := range [][]string{nil, {"-apply"}} {
		err := runWith(t, args...)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to connect to database")
	}
}
