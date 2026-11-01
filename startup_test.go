package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
