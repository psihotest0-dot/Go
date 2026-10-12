package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
)

func NewRiverClient(ctx context.Context, pool *pgxpool.Pool, database *db.Database, llmClient llm.LLMClient, embedder llm.Embedder) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, NewMemoryConsolidationPeriodicWorker(database))
	river.AddWorker(workers, NewMemoryConsolidationWorker(database, llmClient, embedder))
	river.AddWorker(workers, NewEmbeddingCacheGCWorker(database))
	river.AddWorker(workers, NewIngestFileWorker(database, embedder))

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 100},
		},
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(5*time.Minute),
				func() (river.JobArgs, *river.InsertOpts) {
					return MemoryConsolidationPeriodicArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) {
					return EmbeddingCacheGCArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
	if err != nil {
		return nil, err
	}

	return riverClient, nil
}
