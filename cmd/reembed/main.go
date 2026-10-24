package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
)

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	batchSize := flag.Int("batch", 64, "batch size for re-embedding")
	modelName := flag.String("model", "paraphrase-multilingual-MiniLM-L12-v2", "embedding model name")
	filterModel := flag.String("filter-model", "", "model to filter out (only embed if current model != filter-model)")
	flag.Parse()

	if *filterModel == "" {
		*filterModel = *modelName
	}

	cfg := config.Load()
	ctx := context.Background()

	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() {
		database.Pool.Close()
		database.SqlDB.Close()
	}()

	embedder := llm.NewHTTPEmbedder(database)

	limit := int32(*batchSize)

	logger.Info("Starting re-embedding process", slog.Int("batch_size", *batchSize), slog.String("model", *modelName), slog.String("filter_model", *filterModel))

	lastCreatedAt := time.Time{}
	lastID := uuid.Nil

	totalProcessed := 0

	for {
		memories, err := database.Queries.GetAllMemoriesForReembedding(ctx, db.GetAllMemoriesForReembeddingParams{
			LastCreatedAt:  lastCreatedAt,
			LastID:         lastID,
			EmbeddingModel: *filterModel,
			Limit:          limit,
		})
		if err != nil {
			return fmt.Errorf("failed to get memories: %w", err)
		}

		if len(memories) == 0 {
			break
		}

		texts := make([]string, len(memories))
		ids := make([]uuid.UUID, len(memories))
		for i, mem := range memories {
			texts[i] = mem.Content
			ids[i] = mem.ID
		}

		batchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		vectors, err := embedder.EmbedTexts(batchCtx, texts)
		cancel()

		if err != nil {
			return fmt.Errorf("failed to embed texts: %w", err)
		}

		if len(vectors) != len(memories) {
			return fmt.Errorf("embedder returned %d vectors for %d memories, expected exact match", len(vectors), len(memories))
		}

		pgVectors := make([]pgvector.Vector, len(vectors))
		for i, v := range vectors {
			if len(v) != config.VectorDimension {
				return fmt.Errorf("vector for memory %s has incorrect dimensions: expected %d, got %d", ids[i], config.VectorDimension, len(v))
			}
			pgVectors[i] = pgvector.NewVector(v)
		}

		err = database.Queries.UpdateMemoryEmbeddingsBatch(ctx, db.UpdateMemoryEmbeddingsBatchParams{
			EmbeddingModel: *modelName,
			Ids:            ids,
			Embeddings:     pgVectors,
		})
		if err != nil {
			return fmt.Errorf("failed to update memory embeddings batch: %w", err)
		}

		lastMemory := memories[len(memories)-1]
		if lastMemory.CreatedAt.Valid {
			lastCreatedAt = lastMemory.CreatedAt.Time
		}
		lastID = lastMemory.ID

		totalProcessed += len(memories)
		logger.Info("processed batch", slog.Int("batch_size", len(memories)), slog.Int("total", totalProcessed))
	}

	logger.Info("re-embedding process complete", slog.Int("total", totalProcessed))
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", slog.Any("error", err))
		os.Exit(1)
	}
}
