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

	// ToolGroupUnlocks counts the times the model asked for a group of tools that is not on
	// offer from the start (plan item P44): "files" is the one there is.
	ToolGroupUnlocks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_tool_group_unlocks_total",
		Help: "Groups of tools unlocked by the model during a chat turn, by group.",
	}, []string{"group"})

	// ToolCalls counts the calls of the model's tools in the chat, by tool and result: "ok" the
	// tool ran, "refused" the model got the call wrong (a missing argument, a link it did not
	// get from the user; the reason went back to it), "failed" the server failed, "bad_args" the
	// arguments were not a JSON object, "duplicate" the same call (tool and arguments) was made
	// already in the turn and was not run again. The tool is one of the names the server knows, or
	// "unknown" (a name the model made up), so the label cannot grow without bound.
	ToolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_tool_calls_total",
		Help: "Calls of the model's tools during chat turns, by tool and result.",
	}, []string{"tool", "result"})

	// UserFileOps counts what the model's file tools do to the user's files: list, read, write
	// (a new file), append and propose (a write that waits for the user), each ended "ok",
	// "saved", "appended", "created", "not_found", "exists", "refused" (a call the tool
	// would not do: bad name, too big, over a limit) or "failed" (the server). Two more
	// count the checks of what the assistant says: "removal"/"note" (the request asked to
	// delete or rename a file and the model was told it cannot) and "claim"/"corrected" (an
	// answer claimed a change of the files that did not happen and got a correction), and
	// "claim"/"retried" (an answer claimed a write and no file tool was called: the turn was
	// asked again with the file tools, plan item P52). "stall"/"retried" and "stall"/"corrected"
	// are the same for an answer that promised the work for later, or did nothing about a
	// request to write, after no tool was called (plan item P53); "status"/"corrected" is an answer
	// to "успя ли?" that says the work is done while no file of the user was changed (P54);
	// "extraction"/"noted" is a long answer that hands over an extraction of a source after no
	// tool opened one (extraction_claim.go).
	UserFileOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "user_files_operations_total",
		Help: "Operations of the file tools on the user's files, by operation and result.",
	}, []string{"op", "result"})

	// TaskPlanEvents counts what happens to the plans of the tasks (package taskplan): "set" a
	// plan was made, "step_done", "step_failed", "step_skipped" and "step_doing" a step was
	// reported on, "step_refused" a step was claimed done without a call of its tool on record,
	// "completed" the last step was finished, "evidence" a tool call was written to a ledger.
	TaskPlanEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "task_plan_events_total",
		Help: "Events of the plans of the tasks, by event.",
	}, []string{"event"})

	// OracleRequests counts the calls of the trading bot to POST /oracle/evaluate by how they
	// ended. "answered" is a verdict from the model, "cached" one repeated from memory,
	// "failed" a call the model could not answer (the bot treats it as a HOLD),
	// "unauthorized" a wrong or missing token and "bad_request" a body that cannot be used.
	//
	// All three Oracle series carry the mode the bot said it asks in (see OracleMode): the bot
	// asks the same signal in shadow mode for its evaluation and in active mode from a
	// container that follows the verdict, and the one that comes second is answered from
	// what the first caused. The alerts add the modes up (sum), so they do not change.
	OracleRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_requests_total",
		Help: "Calls to the Oracle route by outcome and mode.",
	}, []string{"outcome", "mode"}) // outcome: answered, cached, failed, unauthorized, bad_request

	// OracleVerdicts counts the verdicts the model gave (cached repeats are not counted again),
	// by the mode of the call that made the model give it.
	OracleVerdicts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_verdicts_total",
		Help: "Verdicts given by the Oracle model, by verdict and mode.",
	}, []string{"verdict", "mode"}) // verdict: APPROVE, VETO, HOLD

	// OracleLatency is the time of one call that reached the model, with its repeat, by the
	// mode of the call that started it.
	OracleLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "oracle_latency_seconds",
		Help:    "Time to answer one Oracle call that went to the model, by mode.",
		Buckets: []float64{0.5, 1, 2, 3, 5, 8, 13, 20, 30},
	}, []string{"mode"})

	// PaidLLMTokens counts the tokens a paid LLM provider reported (DeepSeek, for the Oracle
	// when ORACLE_LLM=deepseek): the input that came from its cache, the input that did not,
	// and the output. The money is worked out from these three and the price list.
	PaidLLMTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "paid_llm_tokens_total",
		Help: "Tokens used at a paid LLM provider, by provider and kind.",
	}, []string{"provider", "kind"}) // kind: cache_hit, cache_miss, output

	// PaidLLMRequests counts the requests to a paid LLM provider by the class of the answer
	// ("2xx", "4xx", "5xx") or "network_error".
	PaidLLMRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "paid_llm_requests_total",
		Help: "Requests to a paid LLM provider, by provider and status class.",
	}, []string{"provider", "status"})

	// PaidLLMSpendToday is what the paid model has cost today, estimated at the peak prices
	// of the provider; PaidLLMSpendLimit is the daily limit (PAID_LLM_DAILY_USD). Both are
	// set only when the paid model is on.
	PaidLLMSpendToday = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "paid_llm_spend_today_usd",
		Help: "Estimated cost of the paid LLM today, dollars, at the peak prices.",
	})
	PaidLLMSpendLimit = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "paid_llm_spend_limit_usd",
		Help: "Daily limit of the paid LLM, dollars (PAID_LLM_DAILY_USD).",
	})

	// PaidLLMBalance is the money left on the account of the paid provider, read from its
	// balance endpoint once an hour.
	PaidLLMBalance = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "paid_llm_balance",
		Help: "Balance left at the paid LLM provider, in its currency.",
	}, []string{"currency"})

	// ChatRoutes counts the turns of the chat by the model that answered: "paid" (the reason
	// is "library" for a question to the library, "chosen" for a conversation the owner sent to
	// the cloud), "local" ("default", "chosen_local", or "fallback" after the paid model failed
	// before it did anything) and "paid_failed" (it failed after it had begun). Counted only
	// while the paid provider is configured.
	ChatRoutes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_model_route_total",
		Help: "Chat turns by the model that answered and why.",
	}, []string{"route", "reason"})

	// ChatSessionWaits counts the turns that found their conversation taken by another turn (a
	// message from the app while Telegram's was still being answered, or the other way round) and
	// waited: "served" got its turn, "gave_up" ran out of time or left first. A turn that found the
	// conversation free is not counted. ChatSessionWaitSeconds is how long those waits took.
	ChatSessionWaits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_session_waits_total",
		Help: "Chat turns that had to wait for their conversation to be free, by how the wait ended.",
	}, []string{"result"})
	ChatSessionWaitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_session_wait_seconds",
		Help:    "How long chat turns waited for their conversation to be free.",
		Buckets: []float64{0.5, 2, 5, 15, 30, 60, 120, 180, 300},
	})

	// TelegramMirror counts the answers given in the app to a conversation shared with Telegram
	// that the bot was asked to repeat in the Telegram chat: "sent" (handed to Telegram),
	// "not_allowed" (the chat is not on TELEGRAM_ALLOWED_CHAT_IDS), "not_linked" (the chat is not
	// linked to this user - nothing is sent to a chat that is not the user's) and "failed" (the
	// link could not be read). The texts the server writes on its own (a reminder, a report) count
	// here too, and "held" (it came before the bot was connected and waits for it), "dropped" (more
	// than 50 waited) and "expired" (it waited more than a day).
	TelegramMirror = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "telegram_mirror_total",
		Help: "Answers given in the app that were repeated in the Telegram chat of the conversation, and texts of the server held for the bot.",
	}, []string{"result"}) // result: sent, not_allowed, not_linked, failed, held, dropped, expired

	// WebToolRequests counts the paid web_search / web_fetch calls.
	WebToolRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_web_tool_requests_total",
		Help: "Requests to the paid web search / fetch API, by tool and result.",
	}, []string{"tool", "result"}) // tool: web_search, web_fetch; result: ok, rejected, error

	// LibraryAdd counts the links of the tool add_to_library (plan item P53) by how each
	// ended: "queued" (saved, waiting for the ingest), "duplicate" (the library had the file),
	// or why it was not added: "not_written" (the person did not write that link),
	// "not_public", "blocked" (401/403), "rate_limited" (429), "not_found", "unreachable",
	// "timeout", "too_large", "unsupported", "abstract_only", "bad_url", "empty", "error"
	// (the server failed).
	LibraryAdd = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "library_add_links_total",
		Help: "Links of the add_to_library tool, by result.",
	}, []string{"result"})

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

	// MemoryFactsDropped counts the facts of the background memory analysis that were not stored
	// because they do not stand on the person's words (jobs/fact_evidence.go): no_evidence (the
	// model quoted nothing), not_in_message (its quote is not in the message), name_not_a_name (a
	// fact of the kind "name" whose value is long or is not in the message).
	MemoryFactsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "memory_facts_dropped_total",
		Help: "Facts of the memory analysis dropped because they do not stand on the person's words, by reason.",
	}, []string{"reason"})

	// AnswerPlans counts the turns the plan of an answer to a personal message (П55,
	// internal/answerplan) looked at, by result: "planned", or "skipped_<reason>" - the
	// reasons are answerplan's Skip... values (not_personal, not_due, no_memories, cloud,
	// forced_tool, greeting...). Only while ANSWER_PLAN_ENABLED is on. AnswerPlanMarks counts
	// the memory marks ([1], [2]...) of the planned answers: "used" for a mark of a given
	// memory, "unknown" for a number the plan did not give.
	// HelpAnswers counts the answers of the code to "какво можеш / какви инструменти имаш"
	// (internal/agent/help_answer.go). Only while HELP_ANSWER_ENABLED is on.
	HelpAnswers = promauto.NewCounter(prometheus.CounterOpts{
		Name: "help_answers_total",
		Help: "Answers of the code to a question about what the assistant can do.",
	})

	// ExplainDossiers counts the dossiers of the explanation from the library (step 2.8,
	// internal/agent/explain.go), by result: "built", "empty" (no search found anything),
	// "failed" (every search failed), "scoped" (the question named a file: an ordinary search
	// of that file). Only while EXPLAIN_DOSSIER_ENABLED is on.
	ExplainDossiers = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "explain_dossier_total",
		Help: "Dossiers of the explanation from the library, by result.",
	}, []string{"result"})
	AnswerPlans = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "answer_plan_total",
		Help: "Turns the plan of an answer to a personal message looked at, by result.",
	}, []string{"result"})
	AnswerPlanMarks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "answer_plan_marks_total",
		Help: "Memory marks in planned answers, by kind (used, unknown).",
	}, []string{"kind"})

	// MemoryThemes counts the memories the theme job (step 2.12, jobs.MemoryThemesWorker) looked
	// at, by result: themed (themes stored, the vector made from them), empty (the model gave
	// none), changed (the text changed or the memory went meanwhile), failed (the model or the
	// embedder failed; the next run tries again). Only while MEMORY_THEMES_ENABLED is on.
	MemoryThemes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "memory_themes_total",
		Help: "Memories the theme job looked at, by result (themed, empty, changed, failed).",
	}, []string{"result"})

	// TaskRunSteps counts the steps of the tasks the server walks itself (package taskrun), by kind
	// (key_facts, summary) and result: done (the file is written), failed (with its reason in the
	// plan), stopped (the person stopped the task while the step ran). Only while TASK_RUN_ENABLED.
	TaskRunSteps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "task_run_steps_total",
		Help: "Steps of the tasks the server walks, by kind and result (done, failed, stopped).",
	}, []string{"kind", "result"})

	// TaskRunRetries counts the second requests of a step: the first answer had numbers that are
	// not in the passages of the document.
	TaskRunRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "task_run_retries_total",
		Help: "Second requests of a step of a task, after numbers that are not in the passages.",
	}, []string{"kind"})

	// TaskRunDroppedLines counts the lines left out of the files of the tasks because a number of
	// theirs is not in the passages of the document.
	TaskRunDroppedLines = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "task_run_dropped_lines_total",
		Help: "Lines left out of the files of the tasks: a number of theirs is not in the passages.",
	}, []string{"kind"})

	// TaskRuns counts the tasks the server walks, by kind and event: proposed (the person was
	// asked), started (the person said yes), declined (the person asked for something else),
	// stopped (the person stopped it), done (the report is in the conversation).
	TaskRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "task_runs_total",
		Help: "Tasks the server walks, by kind and event (proposed, started, declined, superseded, stopped, done).",
	}, []string{"kind", "event"})

	// Reminders counts the reminders by event (plan item И8): proposed (said back to the person),
	// corrected_<field> (the code changed a field the model read: date, time, from_now, next_day),
	// kept (the person said yes), declined, problem_<kind>, extract_failed, listed, cancelled,
	// sent_on_time, sent_late.
	Reminders = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reminders_total",
		Help: "Reminders by event (proposed, corrected_*, kept, declined, superseded, problem_*, extract_failed, listed, cancelled, sent_on_time, sent_late).",
	}, []string{"event"})

	// Bills counts the bills read from documents by event: proposed, kept, reminder (a reminder of
	// its due day was kept), declined, paid, same_file, unsupported, unread, no_text, not_a_bill,
	// amount_missing, extract_failed, dropped_<date> (a date the model gave that the text does not hold).
	Bills = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bills_total",
		Help: "Bills read from documents by event (proposed, kept, reminder, declined, superseded, paid, same_file, unread, not_a_bill, amount_missing, dropped_*).",
	}, []string{"event"})

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
	for _, event := range []string{"set", "step_done", "step_failed", "step_skipped", "step_doing", "step_refused", "completed", "evidence"} {
		TaskPlanEvents.WithLabelValues(event)
	}
	for _, mode := range OracleModes {
		OracleLatency.WithLabelValues(mode)
		for _, outcome := range []string{"answered", "cached", "failed", "unauthorized", "bad_request"} {
			OracleRequests.WithLabelValues(outcome, mode)
		}
		for _, verdict := range []string{"APPROVE", "VETO", "HOLD"} {
			OracleVerdicts.WithLabelValues(verdict, mode)
		}
	}
	// The same for the cache, so that the first hit of a kind shows in rate().
	for _, kind := range []string{CacheKindQuery, CacheKindPassage} {
		CacheHits.WithLabelValues(kind)
		CacheMisses.WithLabelValues(kind)
	}
	for _, reason := range []string{"no_evidence", "not_in_message", "name_not_a_name"} {
		MemoryFactsDropped.WithLabelValues(reason)
	}
	// And for the searches, so that the panels show both from the start.
	for _, search := range []string{RetrievalSearchChunks, RetrievalSearchContext} {
		RetrievalNearestDistance.WithLabelValues(search)
		RetrievalResults.WithLabelValues(search)
	}
}

// The values of the mode label of the Oracle series. "none" is a call that did not say (the
// field is optional), and a call refused or unreadable before its mode could be read.
const (
	OracleModeShadow = "shadow"
	OracleModeActive = "active"
	OracleModeNone   = "none"
)

// OracleModes is every value the mode label can have; each series exists at zero from the start.
var OracleModes = []string{OracleModeShadow, OracleModeActive, OracleModeNone}

// OracleMode turns the mode of a request ("shadow", "active", or "" when it did not say) into
// the label of the Oracle series. Anything else is "none": the label never takes a value a
// caller made up, so the number of series is bounded.
func OracleMode(mode string) string {
	switch mode {
	case OracleModeShadow, OracleModeActive:
		return mode
	default:
		return OracleModeNone
	}
}

// The values of the kind label of CacheHits and CacheMisses.
const (
	CacheKindQuery   = "query"
	CacheKindPassage = "passage"
)
