// Command reembed recomputes the vectors in the database with the embedding
// service that is running. Run it after the embedding model changed (and after
// the service was rebuilt): vectors of two models are not comparable, so a
// library searched with the wrong one finds nothing sensible.
//
//	go run ./cmd/reembed -force        # every memory and chunk, whatever it says
//	go run ./cmd/reembed               # only rows not made by the current model
//
// Stop the backend first, or accept that rows it writes meanwhile are done
// again on the next run.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/reembed"
)

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	batchSize := flag.Int("batch", 64, "batch size for re-embedding")
	filterModel := flag.String("filter-model", "", "memories: skip rows already recorded as made by this model (default: the current model)")
	force := flag.Bool("force", false, "recompute every row, whatever model it says it was made by")
	memories := flag.Bool("memories", true, "re-embed memories")
	chunks := flag.Bool("chunks", true, "re-embed document chunks")
	migrate := flag.Bool("migrate", false, "apply database migrations first (run from the repository root)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := config.Load()
	if *migrate {
		if err := db.RunMigrations(cfg.DatabaseURL); err != nil {
			return err
		}
	}

	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer database.Close()

	embedder := llm.NewHTTPEmbedder(database).WithTimeout(cfg.EmbedderTimeout)
	// Refuse to write vectors of the wrong model before touching a single row.
	if err := embedder.CheckModel(ctx); err != nil {
		return err
	}

	opts := reembed.Options{BatchSize: *batchSize, Force: *force, FilterModel: *filterModel, Log: logger}
	logger.Info("starting re-embedding", slog.String("model", config.DefaultEmbedModel), slog.Bool("force", *force))

	if *memories {
		n, err := reembed.Memories(ctx, database, embedder, opts)
		logger.Info("memories done", slog.Int("rewritten", n))
		if err != nil {
			return err
		}
	}
	if *chunks {
		n, err := reembed.Chunks(ctx, database, embedder, opts)
		logger.Info("chunks done", slog.Int("rewritten", n))
		if err != nil {
			return err
		}
	}
	logger.Info("re-embedding complete; rebuild the vector indexes (REINDEX INDEX CONCURRENTLY idx_document_chunks_raw_embedding, idx_document_chunks_concept_embedding, idx_memories_embedding)")
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", slog.Any("error", err))
		os.Exit(1)
	}
}
