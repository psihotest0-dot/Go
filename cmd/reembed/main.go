package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"

	pgvector "github.com/pgvector/pgvector-go"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	batchSize := flag.Int("batch", 64, "batch size for re-embedding")
	modelName := flag.String("model", "paraphrase-multilingual-MiniLM-L12-v2", "embedding model name")
	flag.Parse()

	cfg := config.Load()
	ctx := context.Background()

	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to connect to database", slog.Any("error", err))
		os.Exit(1)
	}
	defer database.Pool.Close()

	embedder := llm.NewHTTPEmbedder(database)

	offset := int32(0)
	limit := int32(*batchSize)

	logger.Info("Starting re-embedding process", slog.Int("batch_size", *batchSize))

	for {
		memories, err := database.Queries.GetAllMemoriesForReembedding(ctx, db.GetAllMemoriesForReembeddingParams{
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			logger.Error("failed to get memories", slog.Any("error", err))
			os.Exit(1)
		}

		if len(memories) == 0 {
			break
		}

		texts := make([]string, len(memories))
		for i, mem := range memories {
			texts[i] = mem.Content
		}

		vectors, err := embedder.EmbedTexts(ctx, texts)
		if err != nil {
			logger.Error("failed to embed texts", slog.Any("error", err))
			os.Exit(1)
		}

		for i, mem := range memories {
			if i < len(vectors) {
				pgVec := pgvector.NewVector(vectors[i])
				err := database.Queries.UpdateMemoryEmbedding(ctx, db.UpdateMemoryEmbeddingParams{
					Embedding:      pgVec,
					EmbeddingModel: *modelName,
					ID:             mem.ID,
				})
				if err != nil {
					logger.Error("failed to update memory embedding", slog.Any("error", err), slog.Any("id", mem.ID))
				}
			}
		}

		offset += limit
		logger.Info("processed batch", slog.Int("offset", int(offset)))
	}

	logger.Info("re-embedding process complete")
}
