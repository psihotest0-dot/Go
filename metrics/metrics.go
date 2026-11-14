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

	// LLMSilentStreams counts chat completions that went silent after they started: the
	// model kept generating but llama-server sent nothing (Gemma 4 repeating its empty
	// thought marker until the token limit, found 2026-11-13). "retried" means nothing
	// had been shown yet and the turn was asked again with thinking on, "kept_partial"
	// that the text shown so far was kept as the answer, and "failed" that thinking was
	// already on, so the turn ended with an error.
	LLMSilentStreams = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_silent_streams_total",
		Help: "Chat completions cut off after the model streamed nothing for too long, by outcome.",
	}, []string{"outcome"}) // outcome: retried, kept_partial, failed

	// Proposals counts the writes the model asked for that wait for the user.
	Proposals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_proposals_total",
		Help: "Rule and memory proposals made by the model, by kind and outcome.",
	}, []string{"kind", "outcome"}) // kind: save_global_rule, create_memory; outcome: created, approved, rejected

	// ChatAttachments counts what happens to the files attached to a conversation:
	// "added" is a new file, "replaced" a new version of one with the same name,
	// "unchanged" the same content again, "read" a part read by the model's tool, and
	// "rejected" an upload that was refused (empty, binary, too large, too many).
	ChatAttachments = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_attachments_total",
		Help: "Files attached to a conversation, by outcome.",
	}, []string{"outcome"}) // outcome: added, replaced, unchanged, read, rejected

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

	// The embedding cache, by the kind of text: "query" is a search question (embedded with the
	// "query: " prefix), "passage" is everything else - the pieces of a book, summaries, memories,
	// category sentences. Split on 2026-11-08 to find out whether the pieces of books are ever
	// found again (plan item P14): they were not, 0.2 % in three days.
	CacheHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Total number of embedding cache hits, by kind of text (query, passage)",
	}, []string{"kind"})

	CacheMisses = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cache_misses_total",
		Help: "Total number of embedding cache misses, by kind of text (query, passage)",
	}, []string{"kind"})

	// The quality of the searches (plan item P30): the distance limits of the search
	// (retrieval.go: 0.20 for passages, 0.23 across languages, 0.17 for the summaries in
	// the prompt) were set by hand on measured questions, and a drift - new books, a new
	// embedding model - showed only in a new measurement. Both are observed after the
	// limits, once per search: a tool call with query_en is two "chunks" searches.
	//
	// RetrievalNearestDistance is the cosine distance of the nearest result that the
	// vector half of a search found. A search that returned nothing, or only what the
	// text half found, adds nothing here; it shows in RetrievalResults.
	RetrievalNearestDistance = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "retrieval_nearest_distance",
		Help:    "Cosine distance of the nearest result a search returned, by search.",
		Buckets: []float64{0.06, 0.08, 0.10, 0.12, 0.14, 0.16, 0.17, 0.18, 0.20, 0.23, 0.25},
	}, []string{"search"})

	// RetrievalResults is how many results a search returned, 0 included.
	RetrievalResults = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "retrieval_results",
		Help:    "Number of results a search returned, by search.",
		Buckets: []float64{0, 1, 2, 3, 4, 5, 6, 8},
	}, []string{"search"})
)

// The values of the search label of RetrievalNearestDistance and RetrievalResults:
// "chunks" is the search of the search_documents tool (passages of the books), "context"
// the search for the concept summaries that go into the chat prompt on every turn.
const (
	RetrievalSearchChunks  = "chunks"
	RetrievalSearchContext = "context"
)

func init() {
	// The Oracle series exist at zero from the start. A counter that first shows up at 1
	// has no earlier sample to grow from, so increase() would miss the first event of its
	// kind, and the alerts on refused or failed calls would miss the first refused or
	// failed call.
	for _, outcome := range []string{"answered", "cached", "failed", "unauthorized", "bad_request"} {
		OracleRequests.WithLabelValues(outcome)
	}
	for _, verdict := range []string{"APPROVE", "VETO", "HOLD"} {
		OracleVerdicts.WithLabelValues(verdict)
	}
	// The same for the cache, so that the first hit of a kind shows in rate().
	for _, kind := range []string{CacheKindQuery, CacheKindPassage} {
		CacheHits.WithLabelValues(kind)
		CacheMisses.WithLabelValues(kind)
	}
	// And for the searches, so that the panels show both from the start.
	for _, search := range []string{RetrievalSearchChunks, RetrievalSearchContext} {
		RetrievalNearestDistance.WithLabelValues(search)
		RetrievalResults.WithLabelValues(search)
	}
}

// The values of the kind label of CacheHits and CacheMisses.
const (
	CacheKindQuery   = "query"
	CacheKindPassage = "passage"
)
