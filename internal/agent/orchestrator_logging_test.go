package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
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

// jsonLogs sends the default logger through the context handler, as main.go
// does, and returns the lines written so far on demand.
func jsonLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(api.NewContextHandler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		var lines []map[string]any
		for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if raw == "" {
				continue
			}
			var line map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &line), raw)
			lines = append(lines, line)
		}
		return lines
	}
}

func findLogLine(t *testing.T, lines []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}
	require.Failf(t, "log line not found", "no %q in %v", msg, lines)
	return nil
}

// The answer was streamed to the user, then the database refused the insert.
// It used to vanish without a trace (`_ = persistAssistantMessage`).
func TestStoreAnswerLogsADatabaseFailure(t *testing.T) {
	lines := jsonLogs(t)
	o := &Orchestrator{db: unreachableDB(t)}
	ctx := context.WithValue(context.Background(), api.RequestIDKey, "req-7")

	o.storeAnswer(ctx, uuid.New(), "session-1", "Отговор, който не се записа", nil, nil)

	line := findLogLine(t, lines(), "failed to store the assistant answer")
	assert.Equal(t, "ERROR", line["level"])
	assert.Equal(t, "session-1", line["session_id"])
	assert.Equal(t, "req-7", line["request_id"], "the line can be found by the id the client received")
	assert.NotEmpty(t, line["err"])
}

func TestTraceLogsADatabaseFailureAsAWarning(t *testing.T) {
	lines := jsonLogs(t)
	o := &Orchestrator{db: unreachableDB(t)}

	o.trace(context.Background(), uuid.New(), "session-2", "search", map[string]any{"hits": 3})

	line := findLogLine(t, lines(), "failed to write the agent trace")
	assert.Equal(t, "WARN", line["level"], "a lost debugging trace is not a server failure")
	assert.Equal(t, "search", line["step"])
	assert.Equal(t, "session-2", line["session_id"])
}

// An answer that is only hidden reasoning is dropped on purpose, before the
// database is touched; that is not a storage failure.
func TestStoreAnswerDoesNotReportAnErrorForAnEmptyAnswer(t *testing.T) {
	lines := jsonLogs(t)
	o := &Orchestrator{db: unreachableDB(t)}

	o.storeAnswer(context.Background(), uuid.New(), "session-3", "<think>само мисли</think>", nil, nil)

	for _, l := range lines() {
		assert.NotEqual(t, "failed to store the assistant answer", l["msg"])
	}
}
