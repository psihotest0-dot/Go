package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// finishedLines returns the "request finished" lines of the log as maps.
func finishedLines(t *testing.T, log string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		if m["msg"] == "request finished" {
			out = append(out, m)
		}
	}
	return out
}

func serveStatus(t *testing.T, status int, path string, sleep time.Duration) string {
	t.Helper()
	logger, buf := newBufferLogger()
	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(sleep)
		w.WriteHeader(status)
	}))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Request-ID", "req-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	return buf.String()
}

func TestAFailedRequestGetsAFinishedLineWithStatusAndTime(t *testing.T) {
	lines := finishedLines(t, serveStatus(t, http.StatusInternalServerError, "/files", 0))
	require.Len(t, lines, 1)
	assert.Equal(t, "WARN", lines[0]["level"], "a 5xx is a warning")
	assert.EqualValues(t, 500, lines[0]["status"])
	assert.Equal(t, "req-1", lines[0]["request_id"], "the id of the incoming request line")
	assert.Equal(t, "/files", lines[0]["path"])
	assert.Contains(t, lines[0], "duration_ms")

	lines = finishedLines(t, serveStatus(t, http.StatusBadRequest, "/files", 0))
	require.Len(t, lines, 1)
	assert.Equal(t, "INFO", lines[0]["level"], "a 4xx is only information")
	assert.EqualValues(t, 400, lines[0]["status"])
}

// Without this the line would double the log: only what is worth searching for is written.
func TestAFastSuccessfulRequestGetsNoFinishedLine(t *testing.T) {
	assert.Empty(t, finishedLines(t, serveStatus(t, http.StatusOK, "/files", 0)))
	assert.Empty(t, finishedLines(t, serveStatus(t, http.StatusNoContent, "/memories", 0)))
}

func TestASlowSuccessfulRequestIsLoggedButNotAChatStream(t *testing.T) {
	previous := slowRequest
	slowRequest = 20 * time.Millisecond
	t.Cleanup(func() { slowRequest = previous })

	lines := finishedLines(t, serveStatus(t, http.StatusOK, "/files", 40*time.Millisecond))
	require.Len(t, lines, 1)
	assert.Equal(t, true, lines[0]["slow"])
	assert.EqualValues(t, 200, lines[0]["status"])

	assert.Empty(t, finishedLines(t, serveStatus(t, http.StatusOK, "/chat/stream", 40*time.Millisecond)),
		"a chat stream runs for tens of seconds by design")
	// ... but a stream that failed is still worth a line.
	assert.Len(t, finishedLines(t, serveStatus(t, http.StatusServiceUnavailable, "/chat/stream", 0)), 1)
}

func TestHealthzNeverGetsALine(t *testing.T) {
	assert.Empty(t, finishedLines(t, serveStatus(t, http.StatusInternalServerError, "/healthz", 0)))
}

func TestAFailedPolledRequestGoesToTheMetricsLog(t *testing.T) {
	mainLogger, mainBuf := newBufferLogger()
	metricsLogger, metricsBuf := newBufferLogger()
	h := Middleware(mainLogger, WithMetricsLogger(metricsLogger))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

	assert.Empty(t, finishedLines(t, mainBuf.String()), "the polling must not bury the main log")
	assert.Len(t, finishedLines(t, metricsBuf.String()), 1)
}

func TestStorageNameKeepsTheExtensionAndDropsWhatWindowsRefuses(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":                 "report.pdf",
		"Книга за морето.epub":       "Книга за морето.epub",
		`C:\Users\x\y.pdf`:           "y.pdf",
		"../../etc/passwd":           "passwd",
		`a:b?.txt`:                   "a_b_.txt",
		`say "hi" <now> | later*.md`: "say _hi_ _now_ _ later_.md",
		"tab\tand\nnewline.txt":      "tab_and_newline.txt",
		"name. . ":                   "name",
		"":                           "file",
		"..":                         "file",
		"dir/":                       "file",
		"???":                        "___",
	} {
		assert.Equal(t, want, storageName(in), "input %q", in)
	}

	long := strings.Repeat("д", 300) + ".epub"
	got := storageName(long)
	assert.LessOrEqual(t, len([]rune(got)), maxStorageNameRunes)
	assert.True(t, strings.HasSuffix(got, ".epub"), "the extension survives the cut")

	noExt := storageName(strings.Repeat("x", 300))
	assert.Len(t, noExt, maxStorageNameRunes)
	// A "extension" that is really most of the name is not kept.
	weird := storageName("a." + strings.Repeat("y", 300))
	assert.LessOrEqual(t, len([]rune(weird)), maxStorageNameRunes)
}

// The case that failed: the rename of the spooled file onto a name Windows does not accept.
func TestAnUploadWithAnAwkwardNameIsStored(t *testing.T) {
	previous := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = previous })

	up, err := storeUpload(strings.NewReader("hello, a plain text file\n"), `a:b?"c".txt`)
	require.NoError(t, err)
	t.Cleanup(up.discard)

	_, statErr := os.Stat(up.path)
	require.NoError(t, statErr)
	assert.True(t, strings.HasSuffix(filepath.Base(up.path), "_a_b__c_.txt"), "stored as %s", filepath.Base(up.path))
}
