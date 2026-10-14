package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"unicode"

	"go-monolith/internal/db"
	"go-monolith/internal/ingest"
	"go-monolith/internal/llm"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
)

type IngestFileArgs struct {
	FileID uuid.UUID `json:"file_id"`
}

func (IngestFileArgs) Kind() string {
	return "ingest_file"
}

type IngestFileWorker struct {
	river.WorkerDefaults[IngestFileArgs]
	dbClient *db.Database
	embedder llm.Embedder
	manager  *ingest.Manager
}

func NewIngestFileWorker(dbClient *db.Database, embedder llm.Embedder) *IngestFileWorker {
	return &IngestFileWorker{
		dbClient: dbClient,
		embedder: embedder,
		manager:  ingest.NewManager(),
	}
}

func (w *IngestFileWorker) Work(ctx context.Context, job *river.Job[IngestFileArgs]) error {
	slog.Info("IngestFileWorker started", "file_id", job.Args.FileID)

	file, err := w.dbClient.Queries.GetIngestedFileByID(ctx, job.Args.FileID)
	if err != nil {
		return fmt.Errorf("failed to get ingested file: %w", err)
	}

	err = w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{
		ID:     file.ID,
		Status: "processing",
	})
	if err != nil {
		return fmt.Errorf("failed to update status to processing: %w", err)
	}

	f, err := os.Open(file.LocalPath)
	if err != nil {
		w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"})
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	text, err := w.manager.Extract(ctx, file.FileName, f)
	if err != nil {
		w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"})
		return fmt.Errorf("failed to extract text: %w", err)
	}


	cyrillicCount := 0
	runes := []rune(text)
	for _, r := range runes {
		if unicode.Is(unicode.Cyrillic, r) {
			cyrillicCount++
		}
	}

	chunkSize := 800
	overlap := 100
	if len(runes) > 0 && float64(cyrillicCount)/float64(len(runes)) > 0.15 {
		chunkSize = 400
		overlap = 50
	}

	chunks := w.chunkText(text, chunkSize, overlap)


	for i, chunk := range chunks {
		vectors, err := w.embedder.EmbedTexts(ctx, []string{chunk})
		if err != nil {
			w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"})
			return fmt.Errorf("failed to embed chunk %d: %w", i, err)
		}
		var vec pgvector.Vector
		if len(vectors) > 0 {
			vec = pgvector.NewVector(vectors[0])
		} else {
			vec = pgvector.NewVector(make([]float32, 384))
		}

		_, err = w.dbClient.Queries.CreateDocumentChunk(ctx, db.CreateDocumentChunkParams{
			FileID:     file.ID,
			UserID:     file.UserID,
			ChunkIndex: int32(i),
			Text:       chunk,
			Embedding:  vec,
		})
		if err != nil {
			w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"})
			return fmt.Errorf("failed to save chunk %d: %w", i, err)
		}
	}

	err = w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{
		ID:     file.ID,
		Status: "completed",
	})
	if err != nil {
		return fmt.Errorf("failed to update status to completed: %w", err)
	}

	slog.Info("IngestFileWorker finished successfully", "file_id", job.Args.FileID, "chunks", len(chunks))
	return nil
}

func (w *IngestFileWorker) chunkText(text string, chunkSize, overlap int) []string {
	runes := []rune(text)
	var chunks []string

	if len(runes) == 0 {
		return chunks
	}

	if chunkSize <= overlap {
		chunkSize = overlap + 1 // Prevent infinite loop
	}

	for i := 0; i < len(runes); i += (chunkSize - overlap) {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
		if end == len(runes) {
			break
		}
	}
	return chunks
}
