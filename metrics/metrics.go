package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	QPS = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "api_requests_total",
		Help: "The total number of processed API requests",
	}, []string{"method", "path", "status"})

	Latency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "api_request_duration_seconds",
		Help:    "API request latency distributions.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	StageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "stage_duration_seconds",
		Help:    "Latency distributions for different stages like embed, search, llm.",
		Buckets: prometheus.DefBuckets,
	}, []string{"stage"})

	TokenCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_tokens_total",
		Help: "Total number of tokens processed by the LLM.",
	}, []string{"type"}) // type: prompt, completion, total

	LLMQueueWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_queue_wait_seconds",
		Help:    "Time LLM requests waited for a free model slot.",
		Buckets: []float64{0.01, 0.1, 0.5, 1, 5, 15, 60, 180, 600},
	}, []string{"priority"}) // priority: interactive, background

	// Proposals counts the writes the model asked for that wait for the user.
	Proposals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_proposals_total",
		Help: "Rule and memory proposals made by the model, by kind and outcome.",
	}, []string{"kind", "outcome"}) // kind: save_global_rule, create_memory; outcome: created, approved, rejected

	// WebToolRequests counts the paid web_search / web_fetch calls.
	WebToolRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_web_tool_requests_total",
		Help: "Requests to the paid web search / fetch API, by tool and result.",
	}, []string{"tool", "result"}) // tool: web_search, web_fetch; result: ok, rejected, error

	CacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Total number of embedding cache hits",
	})

	CacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cache_misses_total",
		Help: "Total number of embedding cache misses",
	})
)
