package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

func capturedLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestEnsureSchemaDoesNothingWhenUpToDate(t *testing.T) {
	for _, auto := range []bool{true, false} {
		logger, _ := capturedLogger()
		applied := false

		err := ensureSchema(logger, auto, db.MigrationStatus{Current: 24}, func() error {
			applied = true
			return nil
		})

		require.NoError(t, err, "AUTO_MIGRATE=%v", auto)
		assert.False(t, applied)
	}
}

func TestEnsureSchemaAppliesPendingMigrationsByDefault(t *testing.T) {
	logger, logs := capturedLogger()
	applied := 0

	err := ensureSchema(logger, true, db.MigrationStatus{Current: 22, Pending: []string{"00023_a.sql", "00024_b.sql"}}, func() error {
		applied++
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, applied)
	assert.Contains(t, logs.String(), "00023_a.sql, 00024_b.sql", "the log names what is about to be applied")
	assert.Contains(t, logs.String(), "backup")
}

func TestEnsureSchemaRefusesToStartWhenMigrationsAreOffAndPending(t *testing.T) {
	logger, _ := capturedLogger()
	applied := false

	err := ensureSchema(logger, false, db.MigrationStatus{Current: 22, Pending: []string{"00023_a.sql"}}, func() error {
		applied = true
		return nil
	})

	require.Error(t, err)
	assert.False(t, applied, "AUTO_MIGRATE=false must never change the schema")
	for _, want := range []string{"AUTO_MIGRATE", "00023_a.sql", "22", "cmd/migrate", "backup"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestEnsureSchemaReportsAFailedMigration(t *testing.T) {
	logger, _ := capturedLogger()
	boom := errors.New("boom")

	err := ensureSchema(logger, true, db.MigrationStatus{Pending: []string{"00001_x.sql"}}, func() error { return boom })

	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "failed to run migrations")
}

func TestPrepareSchemaNeedsTheMigrationsFolder(t *testing.T) {
	t.Chdir(t.TempDir()) // not the repository root
	logger, _ := capturedLogger()

	err := prepareSchema(logger, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository root")
	assert.Contains(t, err.Error(), filepath.Join(mustGetwd(t), "migrations"), "the message says where it looked")
}

func TestLogWorkingPathsNamesAbsoluteFolders(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	logger, logs := capturedLogger()

	logWorkingPaths(logger)

	out := logs.String()
	assert.Contains(t, out, filepath.Join(dir, "migrations"))
	assert.Contains(t, out, filepath.Join(dir, "storage"))
	assert.Contains(t, out, "does not exist yet", "a missing storage folder is a warning: the backend may have been started from another folder")

	require.NoError(t, os.Mkdir(filepath.Join(dir, "storage"), 0o755))
	logger, logs = capturedLogger()
	logWorkingPaths(logger)
	assert.False(t, strings.Contains(logs.String(), "does not exist yet"))
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	return dir
}

// The README starts the backend with `go run main.go`, which compiles that one
// file and nothing else. A second file in this package would make that command
// fail with "undefined: ..." (it did, once), so everything main needs stays in
// main.go. Helpers that can live elsewhere belong in an internal package.
func TestMainPackageIsASingleFile(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	var sources []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			sources = append(sources, f)
		}
	}
	assert.Equal(t, []string{"main.go"}, sources)
}

// waitForOneCompressedBackup waits until lumberjack has finished its work in the
// background (it archives and prunes in a goroutine of its own): exactly one
// backup is left, and it is compressed. Without waiting, a test could end while
// a file is still open and its temporary folder could not be removed.
func waitForOneCompressedBackup(t *testing.T, dir string) {
	t.Helper()
	require.Eventually(t, func() bool {
		backups, _ := filepath.Glob(filepath.Join(dir, "backend-*"))
		return len(backups) == 1 && strings.HasSuffix(backups[0], ".log.gz")
	}, 5*time.Second, 20*time.Millisecond, "exactly one compressed backup is kept")
}

// backend.log must not grow without a limit: at the configured size it is
// compressed and kept as one backup.
func TestLogFileIsRotatedAtTheConfiguredSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.log")
	logFile := newLogFile(path, 1)
	t.Cleanup(func() { _ = logFile.Close() })

	assert.Equal(t, path, logFile.Filename)
	assert.Equal(t, 1, logFile.MaxSize)
	assert.Equal(t, 1, logFile.MaxBackups, "one backup, so the logs take about twice the limit at most")

	line := []byte(strings.Repeat("x", 99) + "\n")
	for written := 0; written < 3<<20; written += len(line) {
		_, err := logFile.Write(line)
		require.NoError(t, err)
	}

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.LessOrEqual(t, info.Size(), int64(1<<20), "the current file stays under the limit")

	waitForOneCompressedBackup(t, dir)
}

// A log that is already over the limit when the backend starts (the limit was
// lowered, or the old build never rotated it) is rotated at once.
func TestOversizedLogFileIsRotatedOnTheFirstWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.log")
	require.NoError(t, os.WriteFile(path, make([]byte, 3<<20), 0o600))
	logFile := newLogFile(path, 1)
	t.Cleanup(func() { _ = logFile.Close() })

	_, err := logFile.Write([]byte("first line\n"))
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Less(t, info.Size(), int64(1<<10))

	waitForOneCompressedBackup(t, dir)
}

// The backup is made by a background goroutine. It must keep working in the
// folder of the log even when the process changes its current directory (a
// test finishing, or any code that calls os.Chdir): this used to leave a 3 MB
// archive of zeros in the project folder and could delete a real one there.
func TestLogFileDoesNotFollowTheCurrentDirectory(t *testing.T) {
	logDir, elsewhere := t.TempDir(), t.TempDir()
	t.Chdir(logDir)
	logFile := newLogFile("backend.log", 1)
	t.Cleanup(func() { _ = logFile.Close() })
	require.True(t, filepath.IsAbs(logFile.Filename), "a relative name is resolved when the file is created")

	t.Chdir(elsewhere)
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "backend.log"), make([]byte, 3<<20), 0o600))
	_, err := logFile.Write([]byte("first line\n"))
	require.NoError(t, err)

	waitForOneCompressedBackup(t, logDir)
	left, err := os.ReadDir(elsewhere)
	require.NoError(t, err)
	assert.Empty(t, left, "nothing is written to the folder the process moved to")
}

// The console shows only errors and the "listening" line, so the warning about
// listening beyond loopback must pass the filter too: in backend.log alone nobody
// would see it (plan item P32). Other warnings stay in the file.
func TestConsoleShowsTheWarningAboutListeningBeyondLoopback(t *testing.T) {
	var file, console bytes.Buffer
	logger := slog.New(&LevelFilterHandler{
		fileHandler:   slog.NewTextHandler(&file, nil),
		stdoutHandler: slog.NewTextHandler(&console, nil),
	})

	logger.Warn("some other warning")
	logger.Warn(api.BeyondLoopbackWarning, slog.String("address", "0.0.0.0:8080"))

	assert.Contains(t, file.String(), "some other warning")
	assert.NotContains(t, console.String(), "some other warning")
	assert.Contains(t, console.String(), api.BeyondLoopbackWarning)
	assert.Contains(t, console.String(), "0.0.0.0:8080")
}
