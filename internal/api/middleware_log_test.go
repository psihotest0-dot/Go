package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newBufferLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

func serveLogged(t *testing.T, handler http.Handler, method, path string) {
	t.Helper()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
}

// The Prometheus scrape (every 15 s) used to fill backend.log with "incoming
// request" lines.
func TestMiddlewareSendsMetricsRequestsToTheirOwnLog(t *testing.T) {
	mainLogger, mainBuf := newBufferLogger()
	metricsLogger, metricsBuf := newBufferLogger()
	handler := Middleware(mainLogger, WithMetricsLogger(metricsLogger))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	serveLogged(t, handler, http.MethodGet, "/metrics")
	serveLogged(t, handler, http.MethodPost, "/chat/stream")

	assert.Contains(t, metricsBuf.String(), `"path":"/metrics"`)
	assert.Contains(t, metricsBuf.String(), `"request_id"`, "the line keeps its request id")
	assert.NotContains(t, metricsBuf.String(), "/chat/stream")

	assert.Contains(t, mainBuf.String(), `"path":"/chat/stream"`)
	assert.NotContains(t, mainBuf.String(), "/metrics", "the scrape must not reach the main log")
}

// Without the option nothing changes: every request is in the one log.
func TestMiddlewareLogsMetricsRequestsToTheMainLogByDefault(t *testing.T) {
	mainLogger, mainBuf := newBufferLogger()
	handler := Middleware(mainLogger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	serveLogged(t, handler, http.MethodGet, "/metrics")

	assert.Contains(t, mainBuf.String(), `"path":"/metrics"`)
}

// /healthz was never logged and still is not, in either log.
func TestMiddlewareStillSkipsHealthz(t *testing.T) {
	mainLogger, mainBuf := newBufferLogger()
	metricsLogger, metricsBuf := newBufferLogger()
	handler := Middleware(mainLogger, WithMetricsLogger(metricsLogger))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	serveLogged(t, handler, http.MethodGet, "/healthz")

	assert.Empty(t, mainBuf.String())
	assert.Empty(t, metricsBuf.String())
}

// Only the exact scrape path is moved: a path that merely starts with it is
// ordinary traffic.
func TestMiddlewareMetricsLogIsOnlyForTheScrapePath(t *testing.T) {
	mainLogger, mainBuf := newBufferLogger()
	metricsLogger, metricsBuf := newBufferLogger()
	handler := Middleware(mainLogger, WithMetricsLogger(metricsLogger))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	serveLogged(t, handler, http.MethodGet, "/metrics/extra")

	assert.Contains(t, mainBuf.String(), "/metrics/extra")
	assert.Empty(t, metricsBuf.String())
}
