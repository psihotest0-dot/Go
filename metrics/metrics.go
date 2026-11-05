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

	// OracleRequests counts the calls of the trading bot to POST /oracle/evaluate by how they
	// ended. "answered" is a verdict from the model, "cached" one repeated from memory,
	// "failed" a call the model could not answer (the bot treats it as a HOLD),
	// "unauthorized" a wrong or missing token and "bad_request" a body that cannot be used.
	OracleRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_requests_total",
		Help: "Calls to the Oracle route by outcome.",
	}, []string{"outcome"}) // outcome: answered, cached, failed, unauthorized, bad_request

	// OracleVerdicts counts the verdicts the model gave (cached repeats are not counted again).
	OracleVerdicts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_verdicts_total",
		Help: "Verdicts given by the Oracle model.",
	}, []string{"verdict"}) // verdict: APPROVE, VETO, HOLD

	// OracleLatency is the time of one call that reached the model, with its repeat.
	OracleLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "oracle_latency_seconds",
		Help:    "Time to answer one Oracle call that went to the model.",
		Buckets: []float64{0.5, 1, 2, 3, 5, 8, 13, 20, 30},
	})

	// WebToolRequests counts the paid web_search / web_fetch calls.
	WebToolRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_web_tool_requests_total",
		Help: "Requests to the paid web search / fetch API, by tool and result.",
	}, []string{"tool", "result"}) // tool: web_search, web_fetch; result: ok, rejected, error

	// IngestJobs counts file ingest attempts by how they ended. "failed" is an attempt
	// River will repeat (or the last one); "cancelled" is input that can never be
	// ingested; "interrupted" is a stop (shutdown, timeout) with attempts left.
	IngestJobs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_jobs_total",
		Help: "File ingest attempts by result.",
	}, []string{"result"}) // result: completed, failed, cancelled, interrupted

	// IngestDuration is how long a file ingest attempt took, by result. A long OCR of
	// a scanned book takes an hour or more, hence the wide buckets.
	IngestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ingest_job_duration_seconds",
		Help:    "Duration of file ingest attempts by result.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400},
	}, []string{"result"})

	// DistillationSummaries counts the concept summaries the background LLM pass made
	// (ok) and the batches it could not (error).
	DistillationSummaries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "distillation_summaries_total",
		Help: "Concept summaries made by the distillation pass, by result.",
	}, []string{"result"}) // result: ok, error

	CacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Total number of embedding cache hits",
	})

	CacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cache_misses_total",
		Help: "Total number of embedding cache misses",
	})
)
