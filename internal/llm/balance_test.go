package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/metrics"
)

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, g.Write(&m))
	return m.GetGauge().GetValue()
}

func TestTheBalanceIsReadFromTheAccount(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		assert.Equal(t, "/user/balance", r.URL.Path)
		writeJSON(w, http.StatusOK, `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"2.37","granted_balance":"0.00","topped_up_balance":"2.37"}]}`)
	}))
	defer srv.Close()
	logger, _ := capturedLogger()

	ok := readBalance(context.Background(), srv.Client(), srv.URL, "the-key", logger)

	require.True(t, ok)
	assert.Equal(t, "Bearer the-key", gotAuth)
	assert.InDelta(t, 2.37, gaugeValue(t, metrics.PaidLLMBalance.WithLabelValues("USD")), 1e-9)
}

func TestABalanceThatCannotBeReadIsLoggedWithoutTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, `{"error":"bad key the-key"}`)
	}))
	defer srv.Close()
	logger, logs := capturedLogger()
	before := counter(t, metrics.PaidLLMRequests.WithLabelValues("deepseek", "balance_error"))

	ok := readBalance(context.Background(), srv.Client(), srv.URL, "the-key", logger)

	assert.False(t, ok)
	assert.Contains(t, logs.String(), "401")
	assert.NotContains(t, logs.String(), "the-key", "neither the key nor the page that echoes it")
	assert.EqualValues(t, 1, counter(t, metrics.PaidLLMRequests.WithLabelValues("deepseek", "balance_error"))-before)
}

func TestABalanceWithABadNumberIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"balance_infos":[{"currency":"USD","total_balance":"lots"}]}`)
	}))
	defer srv.Close()

	_, err := fetchBalance(context.Background(), srv.Client(), srv.URL, "k")

	assert.Error(t, err)
}
