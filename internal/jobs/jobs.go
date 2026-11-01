package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"log/slog"
	"strings"
	"time"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"

	"github.com/google/uuid"
	pgx "github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	openai "github.com/sashabaranov/go-openai"
)

type MemoryConsolidationPeriodicArgs struct{}

func (MemoryConsolidationPeriodicArgs) Kind() string { return "memory_consolidation_periodic" }

type MemoryConsolidationPeriodicWorker struct {
	river.WorkerDefaults[MemoryConsolidationPeriodicArgs]
	dbClient *db.Database
}

func NewMemoryConsolidationPeriodicWorker(dbClient *db.Database) *MemoryConsolidationPeriodicWorker {
	return &MemoryConsolidationPeriodicWorker{dbClient: dbClient}
}

func (w *MemoryConsolidationPeriodicWorker) Work(ctx context.Context, job *river.Job[MemoryConsolidationPeriodicArgs]) error {
	slog.Info("MemoryConsolidationPeriodicWorker started")

	users, err := w.dbClient.Queries.GetUsersWithUnanalyzedMessages(ctx)
	if err != nil {
		return fmt.Errorf("failed to get users with unanalyzed messages: %w", err)
	}

	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("failed to get river client from context: %w", err)
	}

	for _, u := range users {
		res, err := client.Insert(ctx, MemoryConsolidationArgs{
			UserID: u,
		}, memoryConsolidationInsertOpts())
		if err != nil {
			slog.Error("failed to enqueue memory consolidation job for user", "user_id", u, "error", err)
			continue
		}
		if res.UniqueSkippedAsDuplicate {
			slog.Debug("memory consolidation job already queued for user", "user_id", u, "job_id", res.Job.ID)
		}
	}

	return nil
}

// uniqueWhileInFlightStates is the set of job states in which an already
// existing job suppresses a new one with the same args (a memory_consolidation
// job for the same user, a distillation_worker job for the same file).
//
// JobStateCompleted is deliberately absent. River's default state set
// (rivertype.UniqueOptsByStateDefault) includes it, and the partial unique
// index only stops blocking once the stored job leaves every state in its
// unique_states mask. A job that completes while `completed` is part of that
// mask therefore keeps its unique key forever, so every later insert with the
// same args is silently skipped as a duplicate. That turned consolidation into
// a one-shot per user: the first run happened while there was nothing worth
// remembering and no run ever followed it, leaving the memory table empty.
// Dropping `completed` keeps the deduplication that actually matters (never
// more than one queued/running job per user) without locking the user out of
// all future runs.
func uniqueWhileInFlightStates() []rivertype.JobState {
	return []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRunning,
		rivertype.JobStateRetryable,
		rivertype.JobStateScheduled,
	}
}

// memoryConsolidationInsertOpts deduplicates consolidation jobs per user while
// the previous one is still in flight.
func memoryConsolidationInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue: "memory",
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: uniqueWhileInFlightStates(),
		},
	}
}

type MemoryConsolidationArgs struct {
	UserID uuid.UUID `json:"user_id"`
}

func (MemoryConsolidationArgs) Kind() string { return "memory_consolidation" }

// consolidationMessagesPerRun is how many messages of one user are analysed per
// job. The job is queued again every five minutes while messages are waiting.
const consolidationMessagesPerRun = 20

type MemoryConsolidationWorker struct {
	river.WorkerDefaults[MemoryConsolidationArgs]
	dbClient  *db.Database
	llmClient llm.LLMClient
	embedder  llm.Embedder
}

func (w *MemoryConsolidationWorker) Timeout(job *river.Job[MemoryConsolidationArgs]) time.Duration {
	return 15 * time.Minute
}

func NewMemoryConsolidationWorker(dbClient *db.Database, llmClient llm.LLMClient, embedder llm.Embedder) *MemoryConsolidationWorker {
	return &MemoryConsolidationWorker{
		dbClient:  dbClient,
		llmClient: llmClient,
		embedder:  embedder,
	}
}

type Fact struct {
	FactKey          string `json:"fact_key"`
	Value            string `json:"value"`
	Statement        string `json:"statement"`
	IsExplicitChange bool   `json:"is_explicit_change"`
}

func (w *MemoryConsolidationWorker) Work(ctx context.Context, job *river.Job[MemoryConsolidationArgs]) error {
	slog.Info("MemoryConsolidationWorker started", "user_id", job.Args.UserID)

	// Every LLM call below is background work and must leave a slot to the chat.
	ctx = llm.WithBackground(ctx)

	// A candidate (a silent change that contradicts an active fact) is promoted
	// only when the same fact is stated again or changed explicitly: see
	// decideFact. Promoting every candidate on each run made contradicting facts
	// active at the same time.
	conn, err := w.dbClient.SqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection for advisory lock: %w", err)
	}
	defer conn.Close()

	// Use crc32 to convert the UUID string to a safe 32-bit integer lock key
	lockKey := int64(crc32.ChecksumIEEE([]byte(job.Args.UserID.String())))

	var locked bool
	err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&locked)
	if err != nil {
		return fmt.Errorf("failed to get advisory lock: %w", err)
	}
	if !locked {
		slog.Info("MemoryConsolidationWorker skipped (already running for this user)", "user_id", job.Args.UserID)
		return nil
	}

	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, unlockErr := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey)
		if unlockErr != nil {
			slog.Error("failed to release advisory lock", "error", unlockErr)
		}
	}()

	messages, err := w.dbClient.Queries.GetUnanalyzedMessages(ctx, job.Args.UserID)
	if err != nil {
		return fmt.Errorf("failed to get unanalyzed messages: %w", err)
	}

	if len(messages) == 0 {
		return nil
	}

	processedCount := 0

	for _, msg := range messages {
		if ctx.Err() != nil {
			break
		}

		if msg.Role != "user" || strings.TrimSpace(msg.Content) == "" {
			_ = w.dbClient.Queries.MarkMessageAnalyzed(ctx, db.MarkMessageAnalyzedParams{
				ID:     msg.ID,
				UserID: msg.UserID,
			})
			continue
		}

		if msg.AnalysisAttempts >= 3 {
			slog.Warn("skipping permanently failed message",
				"msg_id", msg.ID,
				"attempts", msg.AnalysisAttempts)
			_ = w.dbClient.Queries.MarkMessageAnalyzed(ctx, db.MarkMessageAnalyzedParams{
				ID:     msg.ID,
				UserID: msg.UserID,
			})
			continue
		}

		analyzeCtx, analyzeCancel := context.WithTimeout(ctx, 120*time.Second)
		facts, err := w.analyzeMessage(analyzeCtx, msg.Content)
		analyzeCancel()

		if err != nil {
			slog.Error("failed to analyze message", "msg_id", msg.ID, "error", err)
			_ = w.dbClient.Queries.IncrementAnalysisAttempts(ctx, msg.ID)
			continue
		}

		hasErrors := false
		if len(facts) > 0 {
			for _, fact := range facts {
				processCtx, processCancel := context.WithTimeout(ctx, 30*time.Second)
				err = w.processFact(processCtx, msg, fact)
				processCancel()
				if err != nil {
					slog.Error("failed to process fact", "error", err)
					hasErrors = true
				}
			}
		}

		if hasErrors {
			_ = w.dbClient.Queries.IncrementAnalysisAttempts(ctx, msg.ID)
			continue
		}

		_ = w.dbClient.Queries.MarkMessageAnalyzed(ctx, db.MarkMessageAnalyzedParams{
			ID:     msg.ID,
			UserID: msg.UserID,
		})

		processedCount++
		if processedCount >= consolidationMessagesPerRun {
			break
		}
	}
	return nil
}

func (w *MemoryConsolidationWorker) analyzeMessage(ctx context.Context, content string) ([]Fact, error) {
	systemPrompt, err := prompts.GlobalManager.Render("memory_reflector.tmpl", nil)
	if err != nil {
		slog.Error("failed to render memory_reflector prompt", "err", err)
		systemPrompt = "Extract facts about the user. Return an array of JSON objects with keys: fact_key, value, statement, is_explicit_change."
	}

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: systemPrompt,
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: content,
			},
		},
		MaxTokens:   500,
		Temperature: 0.1,
	}

	resp, err := w.llmClient.ChatWithJSONSchema(ctx, req, []Fact{})
	if err != nil {
		return nil, err
	}

	var facts []Fact
	if len(resp.Choices) > 0 {
		cleaned := strings.TrimSpace(resp.Choices[0].Message.Content)
		cleaned = strings.TrimPrefix(cleaned, "```json")
		cleaned = strings.TrimPrefix(cleaned, "```")
		cleaned = strings.TrimSuffix(cleaned, "```")
		cleaned = strings.TrimSpace(cleaned)

		err = json.Unmarshal([]byte(cleaned), &facts)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal facts: %w", err)
		}
	}
	return facts, nil
}

type EmbeddingCacheGCArgs struct{}

func (EmbeddingCacheGCArgs) Kind() string { return "embedding_cache_gc" }

type EmbeddingCacheGCWorker struct {
	river.WorkerDefaults[EmbeddingCacheGCArgs]
	dbClient *db.Database
}

func NewEmbeddingCacheGCWorker(dbClient *db.Database) *EmbeddingCacheGCWorker {
	return &EmbeddingCacheGCWorker{dbClient: dbClient}
}

func (w *EmbeddingCacheGCWorker) Work(ctx context.Context, job *river.Job[EmbeddingCacheGCArgs]) error {
	slog.Info("EmbeddingCacheGCWorker started")

	for {
		rowsDeleted, err := w.dbClient.Queries.DeleteOldEmbeddingCache(ctx)
		if err != nil {
			return fmt.Errorf("failed to delete old embedding cache: %w", err)
		}
		if rowsDeleted == 0 {
			break
		}
	}

	slog.Info("EmbeddingCacheGCWorker finished")
	return nil
}
