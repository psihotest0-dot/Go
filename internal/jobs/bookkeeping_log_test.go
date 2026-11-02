package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
)

// unreachableDB is a database whose every query fails at once: nothing listens
// on port 1. It needs no running PostgreSQL.
func unreachableDB(t *testing.T) *db.Database {
	t.Helper()
	sqlDB, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &db.Database{SqlDB: sqlDB, Queries: db.New(sqlDB)}
}

func captureLogText(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// If the status cannot be written the file stays "processing" in the library
// for good; this write used to be dropped without a word.
func TestMarkFileFailedLogsWhenTheStatusCannotBeWritten(t *testing.T) {
	logs := captureLogText(t)
	w := &IngestFileWorker{dbClient: unreachableDB(t)}
	fileID := uuid.New()

	w.markFileFailed(fileID, uuid.New(), errors.New("pdf: unsupported"))

	out := logs.String()
	assert.Contains(t, out, "IngestFileWorker failed", "the cause of the failure is still logged first")
	assert.Contains(t, out, "failed to mark the file as failed")
	assert.Contains(t, out, fileID.String())
	assert.Equal(t, 2, strings.Count(out, "level=ERROR"))
}

// A message whose "analysed" flag cannot be saved is analysed again on every run.
func TestAnalysisBookkeepingFailuresAreLogged(t *testing.T) {
	logs := captureLogText(t)
	w := &MemoryConsolidationWorker{dbClient: unreachableDB(t)}
	msgID := uuid.New()

	w.markAnalyzed(context.Background(), msgID, uuid.New())
	w.countFailedAnalysis(context.Background(), msgID)

	out := logs.String()
	assert.Contains(t, out, "failed to mark a message as analysed")
	assert.Contains(t, out, "failed to count a failed analysis attempt")
	assert.Equal(t, 2, strings.Count(out, msgID.String()))
}
