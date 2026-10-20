package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"unicode"

	"go-monolith/internal/db"
	"go-monolith/internal/ingest"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"

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
	catMgr   *retrieval.CategoryManager
}

func NewIngestFileWorker(dbClient *db.Database, embedder llm.Embedder, catMgr *retrieval.CategoryManager) *IngestFileWorker {
	return &IngestFileWorker{
		dbClient: dbClient,
		embedder: embedder,
		manager:  ingest.NewManager(),
		catMgr:   catMgr,
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

	text, err := w.manager.Extract(ctx, file.FileName, f)
	f.Close()
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

	var allVectors [][]float32
	batchSize := 50
	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}

		batchChunks := chunks[i:end]

		var enrichedBatch []string
		for _, chunk := range batchChunks {
			enrichedBatch = append(enrichedBatch, fmt.Sprintf("Файл: %s. Съдържание: %s", file.FileName, chunk))
		}

		vectors, err := w.embedder.EmbedTexts(ctx, enrichedBatch)
		if err != nil {
			w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"})
			return fmt.Errorf("failed to embed batch starting at chunk %d: %w", i, err)
		}

		// Check if vectors match the number of chunks. If not, return an error.
		if len(vectors) != len(enrichedBatch) {
			if updateErr := w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"}); updateErr != nil {
				slog.Error("Failed to update status to failed", "file_id", file.ID, "error", updateErr)
			}
			return fmt.Errorf("mismatch in vector count vs chunks in batch: expected %d, got %d", len(enrichedBatch), len(vectors))
		}

		allVectors = append(allVectors, vectors...)

		var fileIDs []uuid.UUID
		var userIDs []uuid.UUID
		var chunkIndices []int32
		var texts []string
		var embeddings []pgvector.Vector

		for j, chunk := range batchChunks {
			fileIDs = append(fileIDs, file.ID)
			userIDs = append(userIDs, file.UserID)
			chunkIndices = append(chunkIndices, int32(i+j))
			texts = append(texts, chunk)

			if len(vectors[j]) != 384 {
				w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"})
				return fmt.Errorf("invalid vector dimension at chunk %d: %d", i+j, len(vectors[j]))
			}
			embeddings = append(embeddings, pgvector.NewVector(vectors[j]))
		}

		err = w.dbClient.Queries.CreateDocumentChunksBatch(ctx, db.CreateDocumentChunksBatchParams{
			FileIds:      fileIDs,
			UserIds:      userIDs,
			ChunkIndices: chunkIndices,
			Texts:        texts,
			Embeddings:   embeddings,
		})
		if err != nil {
			w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{ID: file.ID, Status: "failed"})
			return fmt.Errorf("failed to save batch starting at chunk %d: %w", i, err)
		}
	}

	if !file.ManualOverride && w.catMgr != nil {
		votes := make(map[string]int)
		for i, _ := range chunks {
			if i >= 30 {
				break
			}
			if i >= len(allVectors) {
				break
			}
			vec := allVectors[i]
			if len(vec) == 0 {
				continue
			}

			bestCat := "general"
			bestScore := float32(-1.0)
			for cat, centroid := range w.catMgr.GetAllCentroids() {
				score := cosineSimilarity(vec, centroid)
				if score > bestScore {
					bestScore = score
					bestCat = cat
				}
			}

			if bestScore > 0.20 {
				votes[bestCat]++
			} else {
				votes["general"]++
			}
		}

		winningCat := "general"
		maxVotes := -1
		for cat, voteCount := range votes {
			if voteCount > maxVotes {
				maxVotes = voteCount
				winningCat = cat
			}
		}

		err = w.dbClient.Queries.UpdateIngestedFileCategoryAuto(ctx, db.UpdateIngestedFileCategoryAutoParams{
			ID:       file.ID,
			Category: winningCat,
		})
		if err != nil {
			slog.Warn("Failed to auto-update file category", "file_id", file.ID, "error", err)
		} else {
			slog.Info("Auto-assigned category to file", "file_id", file.ID, "category", winningCat, "votes", maxVotes)
		}
	}

	err = w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{
		ID:     file.ID,
		Status: "completed",
	})
	if err != nil {
		return fmt.Errorf("failed to update status to completed: %w", err)
	}

	if file.LocalPath != "" {
		if err := os.Remove(file.LocalPath); err != nil {
			slog.Warn("Failed to remove local file after ingestion", "file_id", file.ID, "path", file.LocalPath, "error", err)
		}
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

func cosineSimilarity(a, b []float32) float32 {
	var dotProduct, normA, normB float32
	for i := 0; i < len(a) && i < len(b); i++ {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}
