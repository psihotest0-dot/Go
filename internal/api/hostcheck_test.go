package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serveWithHostCheck(t *testing.T, listenAddr string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(HostCheck(AllowedHosts(listenAddr))(inner))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// get sends a request to srv with the given Host header (httptest speaks to
// 127.0.0.1, but Host is what a rebound page controls).
func get(t *testing.T, srv *httptest.Server, host string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/chat/stream", nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestHostCheckServesTheNamesOfThisMachine(t *testing.T) {
	srv, calls := serveWithHostCheck(t, "127.0.0.1:8080")

	for _, host := range []string{
		"127.0.0.1:8080", "localhost:8080", "LOCALHOST:8080", "localhost.:8080",
		"[::1]:8080", "host.docker.internal:8080", "localhost",
	} {
		assert.Equal(t, http.StatusNoContent, get(t, srv, host), "Host %q is this machine", host)
	}
	assert.Equal(t, 7, *calls)
}

// A page that rebinds its domain to 127.0.0.1 still sends its own domain as Host.
func TestHostCheckRefusesOtherNamesBeforeAnyHandlerRuns(t *testing.T) {
	srv, calls := serveWithHostCheck(t, "127.0.0.1:8080")

	for _, host := range []string{
		"evil.example:8080", "evil.example", "localhost.evil.example:8080",
		"127.0.0.1.evil.example:8080", "192.168.1.5:8080", "[::2]:8080", "localhost@evil.example",
	} {
		assert.Equal(t, http.StatusMisdirectedRequest, get(t, srv, host), "Host %q is not this machine", host)
	}
	assert.Zero(t, *calls, "a refused request never reaches the handler")
}

func TestHostCheckLogsTheRefusedHostTruncated(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	srv, _ := serveWithHostCheck(t, "127.0.0.1:8080")

	get(t, srv, "evil.example:8080")
	assert.Contains(t, buf.String(), `"level":"WARN"`)
	assert.Contains(t, buf.String(), `"host":"evil.example:8080"`)

	buf.Reset()
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'a'
	}
	get(t, srv, string(long))
	assert.Less(t, buf.Len(), 1000, "a huge Host header does not fill the log")
}

func TestAllowedHostsAddsASpecificListenAddress(t *testing.T) {
	assert.Equal(t, loopbackHosts, AllowedHosts("127.0.0.1:8080"), "loopback adds nothing new beyond its own name")
	assert.Contains(t, AllowedHosts("192.168.1.10:8080"), "192.168.1.10")
	assert.Contains(t, AllowedHosts("pc.lan:8080"), "pc.lan")
	assert.Contains(t, AllowedHosts("[fe80::1]:8080"), "fe80::1")

	for _, all := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "garbage"} {
		assert.Equal(t, loopbackHosts, AllowedHosts(all), "%q does not name a host", all)
	}

	srv, _ := serveWithHostCheck(t, "192.168.1.10:8080")
	assert.Equal(t, http.StatusNoContent, get(t, srv, "192.168.1.10:8080"))
	assert.Equal(t, http.StatusMisdirectedRequest, get(t, srv, "192.168.1.11:8080"))
}
