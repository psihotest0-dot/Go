package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requestSeries returns the "method path" label pairs of api_requests_total.
func requestSeries(t *testing.T) map[string]bool {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	series := map[string]bool{}
	for _, family := range families {
		if family.GetName() != "api_requests_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			series[labels["method"]+" "+labels["path"]] = true
		}
	}
	return series
}

// newMetricsChain mirrors the wiring in main.go: a top mux with a catch-all that
// forwards to a user middleware and a second, nested mux.
func newMetricsChain() http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

	protected := http.NewServeMux()
	protected.HandleFunc("/memories", ok)
	protected.HandleFunc("/files/", ok)

	top := http.NewServeMux()
	top.HandleFunc("/healthz", ok)
	top.Handle("/", newUserMiddleware(&countingUpserter{})(RecordRoute(protected)))

	return Middleware(logger)(RecoveryMiddleware(logger)(RecordRoute(top)))
}

func TestMiddlewareMetricsAreBoundedForUnknownPaths(t *testing.T) {
	handler := newMetricsChain()
	before := requestSeries(t)
	userID := uuid.NewString()

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/scan/%d/%s", i, uuid.NewString()), nil)
		req.Header.Set("X-User-ID", userID)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	// Requests without the identity header never reach the nested mux.
	for i := 0; i < 20; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, fmt.Sprintf("/probe-%d", i), nil))
	}
	// HTTP accepts any token as a method.
	for _, method := range []string{"BREW", "PROPFIND", "X-CUSTOM", "SEARCH"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/healthz", nil))
	}

	// 74 requests with 74 distinct URLs/methods may add only these two series.
	allowed := map[string]bool{"GET other": true, "OTHER /healthz": true}
	after := requestSeries(t)
	for s := range after {
		if !before[s] {
			assert.True(t, allowed[s], "unexpected new series %q", s)
		}
	}
	assert.True(t, after["GET other"], "unknown paths must be labelled %q", unmatchedRouteLabel)
	assert.True(t, after["OTHER /healthz"], "unusual methods must collapse into OTHER")
}

func TestMiddlewareMetricsKeepKnownRoutePatterns(t *testing.T) {
	handler := newMetricsChain()
	userID := uuid.NewString()

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "top level route", path: "/healthz", want: "GET /healthz"},
		{name: "nested route", path: "/memories", want: "GET /memories"},
		{name: "nested subtree keeps the pattern, not the id", path: "/files/" + uuid.NewString(), want: "GET /files/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-User-ID", userID)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.True(t, requestSeries(t)[tc.want], "missing series %q", tc.want)
		})
	}
}
