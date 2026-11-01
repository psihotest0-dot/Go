// Package reembed recomputes the vectors stored in the database with the
// embedding service that is running now. It is what cmd/reembed does, kept here
// so that it can be tested: it rewrites every vector of a library.
package reembed

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/ingest"
	"go-monolith/internal/llm"
)

// batchTimeout bounds the embedding of one batch.
const batchTimeout = 2 * time.Minute

// Options tune a run.
type Options struct {
	// BatchSize is how many rows are embedded per request.
	BatchSize int
	// Force recomputes every row, whatever model it says it was made by. The
	// recorded model of memories written before 2026-11-01 cannot be trusted: it
	// says E5 for vectors that a different model produced.
	Force bool
	// FilterModel (memories only) skips rows already recorded as made by this
	// model; empty means config.DefaultEmbedModel. Ignored with Force.
	FilterModel string
	// Log receives progress lines; nil discards them.
	Log *slog.Logger
}

func (o Options) batch() int32 {
	if o.BatchSize <= 0 {
		return 64
	}
	return int32(o.BatchSize)
}

func (o Options) logger() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.New(slog.DiscardHandler)
}

// toVectors validates the embedder's answer and converts it for the database.
func toVectors(vectors [][]float32, want int, what string) ([]pgvector.Vector, error) {
	if len(vectors) != want {
		return nil, fmt.Errorf("embedder returned %d vectors for %d %s", len(vectors), want, what)
	}
	out := make([]pgvector.Vector, len(vectors))
	for i, v := range vectors {
		if len(v) != config.VectorDimension {
			return nil, fmt.Errorf("a vector has %d dimensions, expected %d", len(v), config.VectorDimension)
		}
		out[i] = pgvector.NewVector(v)
	}
	return out, nil
}

// Memories recomputes the vectors of memories and returns how many it rewrote.
func Memories(ctx context.Context, d *db.Database, e llm.Embedder, opts Options) (int, error) {
	log := opts.logger()
	filter := opts.FilterModel
	if filter == "" {
		filter = config.DefaultEmbedModel
	}
	if opts.Force {
		// Every recorded model differs from "", and a missing one counts as different.
		filter = ""
	}

	lastCreatedAt := time.Time{}
	lastID := uuid.Nil
	total := 0
	for {
		memories, err := d.Queries.GetAllMemoriesForReembedding(ctx, db.GetAllMemoriesForReembeddingParams{
			LastCreatedAt:  lastCreatedAt,
			LastID:         lastID,
			EmbeddingModel: filter,
			Limit:          opts.batch(),
		})
		if err != nil {
			return total, fmt.Errorf("failed to get memories: %w", err)
		}
		if len(memories) == 0 {
			return total, nil
		}

		texts := make([]string, len(memories))
		ids := make([]uuid.UUID, len(memories))
		for i, m := range memories {
			texts[i], ids[i] = m.Content, m.ID
		}
		batchCtx, cancel := context.WithTimeout(ctx, batchTimeout)
		vectors, err := e.EmbedTexts(batchCtx, texts, false)
		cancel()
		if err != nil {
			return total, fmt.Errorf("failed to embed memories: %w", err)
		}
		pgVectors, err := toVectors(vectors, len(memories), "memories")
		if err != nil {
			return total, err
		}
		if err := d.Queries.UpdateMemoryEmbeddingsBatch(ctx, db.UpdateMemoryEmbeddingsBatchParams{
			EmbeddingModel: config.DefaultEmbedModel,
			Ids:            ids,
			Embeddings:     pgVectors,
		}); err != nil {
			return total, fmt.Errorf("failed to update memory embeddings: %w", err)
		}

		last := memories[len(memories)-1]
		if last.CreatedAt.Valid {
			lastCreatedAt = last.CreatedAt.Time
		}
		lastID = last.ID
		total += len(memories)
		log.Info("re-embedded memories", slog.Int("batch", len(memories)), slog.Int("total", total))
	}
}

// Chunks recomputes the vectors of document chunks that were not made by the
// current model and returns how many it rewrote. The text that was embedded is
// rebuilt with ingest.EmbeddingText, exactly as ingest and distillation made it.
func Chunks(ctx context.Context, d *db.Database, e llm.Embedder, opts Options) (int, error) {
	log := opts.logger()
	lastID := uuid.Nil
	total := 0
	for {
		chunks, err := d.Queries.GetChunksForReembedding(ctx, db.GetChunksForReembeddingParams{
			LastID:         lastID,
			Force:          opts.Force,
			EmbeddingModel: config.DefaultEmbedModel,
			Limit:          opts.batch(),
		})
		if err != nil {
			return total, fmt.Errorf("failed to get chunks: %w", err)
		}
		if len(chunks) == 0 {
			return total, nil
		}

		texts := make([]string, len(chunks))
		ids := make([]uuid.UUID, len(chunks))
		for i, c := range chunks {
			texts[i] = ingest.EmbeddingText(c.ChunkType, c.FileName, int(c.ChunkIndex), c.Text)
			ids[i] = c.ID
		}
		batchCtx, cancel := context.WithTimeout(ctx, batchTimeout)
		vectors, err := e.EmbedTexts(batchCtx, texts, false)
		cancel()
		if err != nil {
			return total, fmt.Errorf("failed to embed chunks: %w", err)
		}
		pgVectors, err := toVectors(vectors, len(chunks), "chunks")
		if err != nil {
			return total, err
		}
		if err := d.Queries.UpdateChunkEmbeddingsBatch(ctx, db.UpdateChunkEmbeddingsBatchParams{
			EmbeddingModel: config.DefaultEmbedModel,
			Ids:            ids,
			Embeddings:     pgVectors,
		}); err != nil {
			return total, fmt.Errorf("failed to update chunk embeddings: %w", err)
		}

		lastID = chunks[len(chunks)-1].ID
		total += len(chunks)
		log.Info("re-embedded chunks", slog.Int("batch", len(chunks)), slog.Int("total", total))
	}
}
