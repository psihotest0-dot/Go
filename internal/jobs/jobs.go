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

	"database/sql"

	"github.com/google/uuid"
	pgx "github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"
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

	for _, u := range users {
		_, err := river.ClientFromContext[pgx.Tx](ctx).Insert(ctx, MemoryConsolidationArgs{
			UserID: u,
		}, &river.InsertOpts{
			UniqueOpts: river.UniqueOpts{
				ByArgs: true,
			},
		})
		if err != nil {
			slog.Error("failed to enqueue memory consolidation job for user", "user_id", u, "error", err)
		}
	}

	return nil
}

type MemoryConsolidationArgs struct {
	UserID uuid.UUID `json:"user_id"`
}

func (MemoryConsolidationArgs) Kind() string { return "memory_consolidation" }

type MemoryConsolidationWorker struct {
	river.WorkerDefaults[MemoryConsolidationArgs]
	dbClient  *db.Database
	llmClient llm.LLMClient
	embedder  llm.Embedder
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
		_, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey)
		if err != nil {
			slog.Error("failed to release advisory lock", "error", err)
		}
	}()

	messages, err := w.dbClient.Queries.GetUnanalyzedMessages(ctx, job.Args.UserID)
	if err != nil {
		return fmt.Errorf("failed to get unanalyzed messages: %w", err)
	}

	if len(messages) == 0 {
		return nil
	}

	for i, msg := range messages {
		if i >= 10 {
			break
		}

		if msg.Role != "user" || strings.TrimSpace(msg.Content) == "" {
			continue
		}

		msgCtx, cancel := context.WithTimeout(ctx, 60*time.Second)

		facts, err := w.analyzeMessage(msgCtx, msg.Content)
		if err != nil {
			slog.Error("failed to analyze message", "msg_id", msg.ID, "error", err)
			cancel()
			continue
		}

		if len(facts) == 0 {
			err = w.processFact(msgCtx, msg, Fact{
				FactKey:          "__dummy__",
				Value:            "",
				Statement:        "",
				IsExplicitChange: false,
			})
			if err != nil {
				slog.Error("failed to process dummy fact", "error", err)
			}
			cancel()
			continue
		}

		for _, fact := range facts {
			err = w.processFact(msgCtx, msg, fact)
			if err != nil {
				slog.Error("failed to process fact", "fact", fact, "error", err)
			}
		}
		cancel()
	}
	return nil
}

func (w *MemoryConsolidationWorker) analyzeMessage(ctx context.Context, content string) ([]Fact, error) {
	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "Extract facts about the user. Return an array of JSON objects with keys: fact_key (e.g. location, name), value, statement (the exact fact sentence), is_explicit_change (boolean indicating if the user explicitly changed a previous fact, like 'I moved to').",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: content,
			},
		},
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

func (w *MemoryConsolidationWorker) processFact(ctx context.Context, msg db.Message, fact Fact) error {
	if strings.TrimSpace(fact.Statement) == "" && fact.FactKey != "__dummy__" {
		return nil
	}

	tx, err := w.dbClient.SqlDB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	q := w.dbClient.Queries.WithTx(tx)

	if fact.FactKey == "__dummy__" {
		nullMsgID := uuid.NullUUID{UUID: msg.ID, Valid: true}
		h := fmt.Sprintf("dummy-%s", msg.ID.String())
		vec := pgvector.NewVector(make([]float32, 384))
		_, err = q.CreateMemory(ctx, db.CreateMemoryParams{
			UserID:          msg.UserID,
			Content:         "[analyzed]",
			Status:          "superseded",
			Embedding:       vec,
			EmbeddingModel:  "none",
			SourceMessageID: nullMsgID,
			Importance:      0,
			Metadata:        pqtype.NullRawMessage{},
			ContentHash:     h,
		})
		if err != nil {
			return err
		}
		return tx.Commit()
	}

	memories, err := q.GetMemories(ctx, db.GetMemoriesParams{
		UserID: msg.UserID,
	})
	if err != nil {
		return err
	}

	var conflict *db.Memory
	for _, m := range memories {
		if m.Status == "active" || m.Status == "candidate" {
			var meta map[string]interface{}
			if m.Metadata.Valid {
				_ = json.Unmarshal(m.Metadata.RawMessage, &meta)
				if fk, ok := meta["fact_key"].(string); ok && fk == fact.FactKey {
					cm := m
					conflict = &cm
					break
				}
			}
		}
	}

	status := "candidate"
	if conflict != nil {
		if fact.IsExplicitChange {
			status = "active"
			err = q.SupercedeMemory(ctx, db.SupercedeMemoryParams{
				ID:     conflict.ID,
				UserID: msg.UserID,
			})
			if err != nil {
				return err
			}
		} else {
			status = "candidate"
		}
	} else {
		status = "candidate"
	}

	vectors, err := w.embedder.EmbedTexts(ctx, []string{fact.Statement})
	if err != nil {
		return err
	}
	var vec pgvector.Vector
	if len(vectors) > 0 {
		vec = pgvector.NewVector(vectors[0])
	} else {
		vec = pgvector.NewVector(make([]float32, 384))
	}

	metaBytes, _ := json.Marshal(map[string]interface{}{
		"fact_key":           fact.FactKey,
		"value":              fact.Value,
		"is_explicit_change": fact.IsExplicitChange,
	})

	nullMsgID := uuid.NullUUID{UUID: msg.ID, Valid: true}

	h := fmt.Sprintf("%s-%s-%d", fact.Statement, msg.ID.String(), time.Now().UnixNano())

	_, err = q.CreateMemory(ctx, db.CreateMemoryParams{
		UserID:          msg.UserID,
		Content:         fact.Statement,
		Status:          status,
		Embedding:       vec,
		EmbeddingModel:  "paraphrase-multilingual-MiniLM-L12-v2",
		SourceMessageID: nullMsgID,
		Importance:      1,
		Metadata:        pqtype.NullRawMessage{RawMessage: metaBytes, Valid: true},
		ContentHash:     h,
	})
	if err != nil {
		return err
	}

	return tx.Commit()
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
