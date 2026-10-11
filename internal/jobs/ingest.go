package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"go-monolith/internal/db"
	"go-monolith/internal/ingest"
	"go-monolith/internal/llm"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
)

type IngestFileJobArgs struct {
	FileID uuid.UUID `json:"file_id"`
}

func (IngestFileJobArgs) Kind() string { return "ingest_file" }

type IngestFileWorker struct {
	river.WorkerDefaults[IngestFileJobArgs]
	dbClient *db.Database
	embedder llm.Embedder
}

func NewIngestFileWorker(dbClient *db.Database, embedder llm.Embedder) *IngestFileWorker {
	return &IngestFileWorker{
		dbClient: dbClient,
		embedder: embedder,
	}
}

func (w *IngestFileWorker) Work(ctx context.Context, job *river.Job[IngestFileJobArgs]) error {
	slog.Info("IngestFileWorker started", "file_id", job.Args.FileID)

	// Get file from DB
	dbFile, err := w.dbClient.Queries.GetIngestedFile(ctx, job.Args.FileID)
	if err != nil {
		return fmt.Errorf("failed to get file from db: %w", err)
	}

	// Update status to processing
	err = w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{
		ID:     job.Args.FileID,
		Status: "processing",
	})
	if err != nil {
		slog.Error("failed to update status", "error", err)
	}

	// Read physical file
	matches, err := filepath.Glob(filepath.Join("storage", dbFile.ID.String()+".*"))
	var filePath string
	if err == nil && len(matches) > 0 {
		filePath = matches[0]
	} else {
		filePath = filepath.Join("storage", dbFile.ID.String())
	}

	file, err := os.Open(filePath)
	if err != nil {
		w.setFailedStatus(ctx, job.Args.FileID)
		return fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer file.Close()

	// Extract text
	analyzer := ingest.NewAnalyzer()
	extractedText, err := analyzer.Analyze(ctx, file, dbFile.FileName)
	if err != nil {
		w.setFailedStatus(ctx, job.Args.FileID)
		return fmt.Errorf("failed to analyze file: %w", err)
	}

	// Chunk text
	chunks := ingest.ChunkText(extractedText)

	// Embed and Save
	for i, chunkText := range chunks {
		vectors, err := w.embedder.EmbedTexts(ctx, []string{chunkText})
		if err != nil {
			slog.Error("failed to embed chunk", "file_id", job.Args.FileID, "chunk_index", i, "error", err)
			continue
		}

		var vec pgvector.Vector
		if len(vectors) > 0 {
			vec = pgvector.NewVector(vectors[0])
		} else {
			vec = pgvector.NewVector(make([]float32, 384))
		}

		_, err = w.dbClient.Queries.CreateDocumentChunk(ctx, db.CreateDocumentChunkParams{
			FileID:     job.Args.FileID,
			UserID:     dbFile.UserID,
			ChunkIndex: int32(i),
			Text:       chunkText,
			Embedding:  vec,
		})
		if err != nil {
			slog.Error("failed to save chunk", "file_id", job.Args.FileID, "chunk_index", i, "error", err)
		}
	}

	// Update status to completed
	err = w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{
		ID:     job.Args.FileID,
		Status: "completed",
	})
	if err != nil {
		return fmt.Errorf("failed to update status to completed: %w", err)
	}

	slog.Info("IngestFileWorker finished", "file_id", job.Args.FileID)
	return nil
}

func (w *IngestFileWorker) setFailedStatus(ctx context.Context, fileID uuid.UUID) {
	_ = w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{
		ID:     fileID,
		Status: "failed",
	})
}
