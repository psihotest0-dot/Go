package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"go-monolith/internal/config"
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

func (w *IngestFileWorker) Timeout(job *river.Job[IngestFileArgs]) time.Duration {
	return 1 * time.Hour // Large documents and OCR can take a significant amount of time
}

func NewIngestFileWorker(dbClient *db.Database, embedder llm.Embedder, catMgr *retrieval.CategoryManager) *IngestFileWorker {
	return &IngestFileWorker{
		dbClient: dbClient,
		embedder: embedder,
		manager:  ingest.NewManager(),
		catMgr:   catMgr,
	}
}

func (w *IngestFileWorker) markFileFailed(fileID uuid.UUID, userID uuid.UUID, err error) {
	slog.Error("IngestFileWorker failed", "file_id", fileID, "error", err)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = w.dbClient.Queries.UpdateIngestedFileStatus(cleanupCtx, db.UpdateIngestedFileStatusParams{
		ID:     fileID,
		Status: "failed",
		UserID: userID,
	})
}

func (w *IngestFileWorker) Work(ctx context.Context, job *river.Job[IngestFileArgs]) error {
	slog.Info("IngestFileWorker started", "file_id", job.Args.FileID)

	_, err := w.dbClient.SqlDB.ExecContext(ctx, "DELETE FROM document_chunks WHERE file_id = $1", job.Args.FileID)
	if err != nil {
		return fmt.Errorf("failed to clean up old document chunks: %w", err)
	}

	file, err := w.dbClient.Queries.GetIngestedFileByID(ctx, job.Args.FileID)
	if err != nil {
		return fmt.Errorf("failed to get ingested file: %w", err)
	}

	err = w.dbClient.Queries.UpdateIngestedFileStatus(ctx, db.UpdateIngestedFileStatusParams{
		ID:     file.ID,
		Status: "processing",
		UserID: file.UserID,
	})
	if err != nil {
		return fmt.Errorf("failed to update status to processing: %w", err)
	}

	f, err := os.Open(file.LocalPath)
	if err != nil {
		w.markFileFailed(file.ID, file.UserID, err)
		return fmt.Errorf("failed to open file: %w", err)
	}

	text, err := w.manager.Extract(ctx, file.FileName, f)
	f.Close()
	if err != nil {
		w.markFileFailed(file.ID, file.UserID, err)
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
		for k, chunk := range batchChunks {
			enrichedBatch = append(enrichedBatch, fmt.Sprintf("[Файл: %s - Chunk %d] %s", file.FileName, i+k, chunk))
		}

		vectors, embedErr := w.embedder.EmbedTexts(ctx, enrichedBatch, false)
		if embedErr != nil {
			w.markFileFailed(file.ID, file.UserID, embedErr)
			return fmt.Errorf("failed to embed batch starting at chunk %d: %w", i, embedErr)
		}

		// Check if vectors match the number of chunks. If not, return an error.
		if len(vectors) != len(enrichedBatch) {
			err = fmt.Errorf("mismatch in vector count vs chunks in batch: expected %d, got %d", len(enrichedBatch), len(vectors))
			w.markFileFailed(file.ID, file.UserID, err)
			return err
		}

		allVectors = append(allVectors, vectors...)

		var fileIDs []uuid.UUID
		var userIDs []uuid.UUID
		var chunkIndices []int32
		var texts []string
		var embeddings []pgvector.Vector
		var chunkTypes []string
		var metadatas []string

		for j, chunk := range batchChunks {
			fileIDs = append(fileIDs, file.ID)
			userIDs = append(userIDs, file.UserID)
			chunkIndices = append(chunkIndices, int32(i+j))
			texts = append(texts, chunk)
			chunkTypes = append(chunkTypes, "raw_text")
			metadatas = append(metadatas, "{}")

			if len(vectors[j]) != config.VectorDimension {
				err = fmt.Errorf("invalid vector dimension at chunk %d: %d", i+j, len(vectors[j]))
				w.markFileFailed(file.ID, file.UserID, err)
				return err
			}
			embeddings = append(embeddings, pgvector.NewVector(vectors[j]))
		}

		if len(fileIDs) != len(texts) ||
		   len(userIDs) != len(texts) ||
		   len(chunkIndices) != len(texts) ||
		   len(embeddings) != len(texts) ||
		   len(chunkTypes) != len(texts) ||
		   len(metadatas) != len(texts) {
		    err = fmt.Errorf(
		        "array length mismatch: fileIDs=%d userIDs=%d chunkIndices=%d texts=%d embeddings=%d chunkTypes=%d metadatas=%d",
		        len(fileIDs), len(userIDs), len(chunkIndices), len(texts),
		        len(embeddings), len(chunkTypes), len(metadatas),
		    )
		    w.markFileFailed(file.ID, file.UserID, err)
		    return err
		}

		err = w.dbClient.Queries.CreateDocumentChunksBatch(ctx, db.CreateDocumentChunksBatchParams{
			FileIds:      fileIDs,
			UserIds:      userIDs,
			ChunkIndices: chunkIndices,
			Texts:        texts,
			Embeddings:   embeddings,
			ChunkTypes:   chunkTypes,
			Metadatas:    metadatas,
		})
		if err != nil {
			w.markFileFailed(file.ID, file.UserID, err)
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
		UserID: file.UserID,
	})
	if err != nil {
		return fmt.Errorf("failed to update status to completed: %w", err)
	}

	if file.LocalPath != "" {
		if err := os.Remove(file.LocalPath); err != nil {
			slog.Warn("Failed to remove local file after ingestion", "file_id", file.ID, "path", file.LocalPath, "error", err)
		}
	}

	// Enqueue distillation worker
	if config.Load().EnableDistillation {
		pgxClient := river.ClientFromContext[pgx.Tx](ctx)
		if pgxClient != nil {
			_, err = pgxClient.Insert(ctx, DistillationWorkerArgs{FileID: file.ID},
				&river.InsertOpts{
					Queue: "distillation",
				})
			if err != nil {
				slog.Error("Failed to enqueue DistillationWorkerArgs", "file_id", file.ID, "error", err)
			}
		} else {
			slog.Warn("Could not enqueue DistillationWorkerArgs: no river pgx client in context")
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

	// Pre-process runes to strip pure noise
	for i := 0; i < len(runes); i += (chunkSize - overlap) {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}

		chunkStr := string(runes[i:end])

		// Soft filter: if chunk is predominantly dots/noise and lacks alpha chars, it's likely a ToC or filler.
		dotCount := strings.Count(chunkStr, ".")
		alphaCount := 0
		for _, c := range chunkStr {
			if unicode.IsLetter(c) {
				alphaCount++
			}
		}

		// If more than half the chunk is dots and there are very few letters, we drop it (noise)
		isNoise := false
		if utf8.RuneCountInString(chunkStr) > 0 {
			if float64(dotCount)/float64(utf8.RuneCountInString(chunkStr)) > 0.5 && alphaCount < 10 {
				isNoise = true
			}
		}

		if !isNoise {
			chunks = append(chunks, chunkStr)
		}

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
