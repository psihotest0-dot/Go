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
	OracleRequests.WithLabelValues("answered").Inc()
	OracleVerdicts.WithLabelValues("HOLD").Inc()
	OracleLatency.Observe(1)
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
		"oracle_requests_total",
		"oracle_verdicts_total",
		"oracle_latency_seconds",
		"ingest_jobs_total",
		"ingest_job_duration_seconds",
		"distillation_summaries_total",
		"cache_hits_total",
		"cache_misses_total",
	} {
		assert.True(t, got[name], "metric %s is not exported", name)
	}
}

// The Oracle alerts use increase(); a counter that first appears at 1 would hide its first
// event from them, so every outcome and verdict is exported at zero from the start.
func TestOracleSeriesExistFromTheStart(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	labelValues := func(metric, label string) map[string]bool {
		got := map[string]bool{}
		for _, family := range families {
			if family.GetName() != metric {
				continue
			}
			for _, m := range family.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == label {
						got[l.GetValue()] = true
					}
				}
			}
		}
		return got
	}

	outcomes := labelValues("oracle_requests_total", "outcome")
	for _, want := range []string{"answered", "cached", "failed", "unauthorized", "bad_request"} {
		assert.True(t, outcomes[want], "oracle_requests_total{outcome=%q} is not exported", want)
	}
	verdicts := labelValues("oracle_verdicts_total", "verdict")
	for _, want := range []string{"APPROVE", "VETO", "HOLD"} {
		assert.True(t, verdicts[want], "oracle_verdicts_total{verdict=%q} is not exported", want)
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
