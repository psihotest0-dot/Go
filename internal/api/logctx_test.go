package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureDefaultLogger routes the default slog logger through the context
// handler into a buffer, the way main.go sets it up.
func captureDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(NewContextHandler(slog.NewJSONHandler(buf, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// logLines decodes the JSON lines the logger wrote.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
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

func findLine(t *testing.T, lines []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}
	require.Failf(t, "log line not found", "no %q in %v", msg, lines)
	return nil
}

func TestContextHandlerAddsRequestAndUserID(t *testing.T) {
	buf := captureDefaultLogger(t)
	user := uuid.New()
	ctx := context.WithValue(context.Background(), RequestIDKey, "req-1")
	ctx = context.WithValue(ctx, ParsedUserIDKey, user)

	slog.ErrorContext(ctx, "something failed", "error", "boom")

	line := findLine(t, logLines(t, buf), "something failed")
	assert.Equal(t, "req-1", line["request_id"])
	assert.Equal(t, user.String(), line["user_id"])
	assert.Equal(t, "boom", line["error"])
}

func TestContextHandlerLeavesPlainLogCallsAlone(t *testing.T) {
	buf := captureDefaultLogger(t)

	slog.Error("startup problem")
	slog.ErrorContext(context.Background(), "background problem")

	for _, line := range logLines(t, buf) {
		assert.NotContains(t, line, "request_id")
		assert.NotContains(t, line, "user_id")
	}
}

// The raw X-User-ID header is client text of any length; only the parsed uuid
// may reach the log through the context.
func TestContextHandlerIgnoresTheRawUserHeader(t *testing.T) {
	buf := captureDefaultLogger(t)
	ctx := context.WithValue(context.Background(), UserIDKey, strings.Repeat("x", 5000))

	slog.WarnContext(ctx, "odd header")

	assert.NotContains(t, findLine(t, logLines(t, buf), "odd header"), "user_id")
}

func TestContextHandlerSurvivesWithAttrsAndWithGroup(t *testing.T) {
	buf := captureDefaultLogger(t)
	ctx := context.WithValue(context.Background(), RequestIDKey, "req-2")

	slog.With("component", "api").ErrorContext(ctx, "with attrs")

	line := findLine(t, logLines(t, buf), "with attrs")
	assert.Equal(t, "api", line["component"])
	assert.Equal(t, "req-2", line["request_id"])
}

// What the failure-injection run showed: the client got an X-Request-ID, but
// the ERROR/WARN lines of the handlers did not carry it. This runs the real
// chain and checks the id the client holds is the id in the log.
func TestHandlerErrorLogsCarryTheClientsRequestID(t *testing.T) {
	buf := captureDefaultLogger(t)
	user := uuid.New()
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boom":
			writeInternalError(w, r, "failed to get memories", errors.New("connection refused"))
		default:
			writeInvalidBody(w, r, errors.New("invalid character"))
		}
	})
	handler := Middleware(slog.Default())(newUserMiddleware(&countingUpserter{})(failing))

	for _, path := range []string{"/boom", "/bad"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("X-User-ID", user.String())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		requestID := rec.Header().Get("X-Request-ID")
		require.NotEmpty(t, requestID)
		msg := map[string]string{"/boom": "failed to get memories", "/bad": "invalid request body"}[path]
		line := findLine(t, logLines(t, buf), msg)
		assert.Equal(t, requestID, line["request_id"], path)
		assert.Equal(t, user.String(), line["user_id"], path)
		assert.Equal(t, path, line["path"], path)
	}
}

func TestUserMiddlewareWarningsCarryTheRequestID(t *testing.T) {
	buf := captureDefaultLogger(t)
	handler := Middleware(slog.Default())(newUserMiddleware(&countingUpserter{})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

	req := httptest.NewRequest(http.MethodGet, "/memories", nil)
	req.Header.Set("X-User-ID", strings.Repeat("n", 500))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	line := findLine(t, logLines(t, buf), "invalid X-User-ID header")
	assert.Equal(t, rec.Header().Get("X-Request-ID"), line["request_id"])
	assert.LessOrEqual(t, len([]rune(line["header"].(string))), maxLoggedHeaderRunes+3, "a long header is cut in the log")
}
