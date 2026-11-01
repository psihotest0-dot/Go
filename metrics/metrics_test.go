package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The dashboard (grafana/dashboards/main.json) and the alert rules
// (prometheus/alerts.yml) query these names, so renaming one silently blanks a
// panel or turns an alert off. This test is the reminder to change them together.
func TestMetricNamesThatDashboardsAndAlertsRelyOn(t *testing.T) {
	QPS.WithLabelValues("GET", "/x", "200").Inc()
	Latency.WithLabelValues("GET", "/x").Observe(0.01)
	StageLatency.WithLabelValues("search").Observe(0.01)
	TokenCount.WithLabelValues("total").Add(1)
	LLMQueueWait.WithLabelValues("interactive").Observe(0.01)
	Proposals.WithLabelValues("save_global_rule", "created").Inc()
	WebToolRequests.WithLabelValues("web_search", "ok").Inc()
	IngestJobs.WithLabelValues("completed").Inc()
	IngestDuration.WithLabelValues("completed").Observe(1)
	DistillationSummaries.WithLabelValues("ok").Inc()
	CacheHits.Inc()
	CacheMisses.Inc()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	got := map[string]bool{}
	for _, family := range families {
		got[family.GetName()] = true
	}

	for _, name := range []string{
		"api_requests_total",
		"api_request_duration_seconds",
		"stage_duration_seconds",
		"llm_tokens_total",
		"llm_queue_wait_seconds",
		"agent_proposals_total",
		"agent_web_tool_requests_total",
		"ingest_jobs_total",
		"ingest_job_duration_seconds",
		"distillation_summaries_total",
		"cache_hits_total",
		"cache_misses_total",
	} {
		assert.True(t, got[name], "metric %s is not exported", name)
	}
}

func TestCountersOnlyGoUp(t *testing.T) {
	before := counterValue(t, "cache_hits_total")

	CacheHits.Inc()
	CacheHits.Inc()

	assert.Equal(t, before+2, counterValue(t, "cache_hits_total"))
}

func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) > 0 {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}
