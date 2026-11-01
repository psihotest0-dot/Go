package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	"go-monolith/metrics"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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
	// distillation is ENABLE_DISTILLATION, read once when the worker is built.
	distillation bool
	// keepOriginals is KEEP_ORIGINALS, read once when the worker is built: whether
	// the uploaded file stays in storage/ after a successful ingest.
	keepOriginals bool
}

// ingestJobTimeout is only a ceiling for a job that is stuck. A PDF sets its own,
// much tighter deadline from its page count (ingest.pdfDeadline), which is what
// ends a slow OCR; an hour was too little for a long scanned book.
const ingestJobTimeout = 12 * time.Hour

func (w *IngestFileWorker) Timeout(job *river.Job[IngestFileArgs]) time.Duration {
	return ingestJobTimeout
}

// ocrCheckpointDir is where the OCR of a file keeps its finished pages between
// attempts. It is in the temp directory on purpose: it is only a way to avoid
// repeating work, and losing it costs time, not data.
func ocrCheckpointDir(fileID uuid.UUID) string {
	return filepath.Join(os.TempDir(), "go-monolith-ocr", fileID.String())
}

func NewIngestFileWorker(dbClient *db.Database, embedder llm.Embedder, catMgr *retrieval.CategoryManager) *IngestFileWorker {
	cfg := config.Load()
	return &IngestFileWorker{
		dbClient: dbClient,
		embedder: embedder,
		manager: ingest.NewManager(ingest.WithLimits(ingest.Limits{
			MaxInputBytes: cfg.MaxUploadBytes,
		})),
		catMgr:        catMgr,
		distillation:  cfg.EnableDistillation,
		keepOriginals: cfg.KeepOriginals,
	}
}

// fail records an ingest failure and returns the error River should see.
//
// Input that can never be ingested (empty, unsupported, too large) is cancelled,
// so River does not run the same doomed job up to 25 times. When the context is
// done (shutdown, job timeout) and River will try again, the file stays
// "processing": marking it failed would show a transient state as the outcome.
func (w *IngestFileWorker) fail(ctx context.Context, job *river.Job[IngestFileArgs], file db.IngestedFile, err error) error {
	if ctx.Err() != nil && job.Attempt < job.MaxAttempts {
		slog.Warn("IngestFileWorker interrupted, the job will be retried", "file_id", file.ID, "error", err)
		return err
	}
	w.markFileFailed(file.ID, file.UserID, err)
	if ingest.IsPermanent(err) {
		return river.JobCancel(err)
	}
	return err
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

// ingestResult names how an ingest attempt ended, for the metrics.
func ingestResult(ctx context.Context, err error) string {
	var cancelled *rivertype.JobCancelError
	switch {
	case err == nil:
		return "completed"
	case errors.As(err, &cancelled):
		return "cancelled"
	case ctx.Err() != nil:
		return "interrupted"
	default:
		return "failed"
	}
}

func (w *IngestFileWorker) Work(ctx context.Context, job *river.Job[IngestFileArgs]) error {
	start := time.Now()
	err := w.work(ctx, job)

	result := ingestResult(ctx, err)
	metrics.IngestJobs.WithLabelValues(result).Inc()
	metrics.IngestDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
	return err
}

func (w *IngestFileWorker) work(ctx context.Context, job *river.Job[IngestFileArgs]) error {
	slog.Info("IngestFileWorker started", "file_id", job.Args.FileID)

	file, err := w.dbClient.Queries.GetIngestedFileByID(ctx, job.Args.FileID)
	if errors.Is(err, sql.ErrNoRows) {
		// The row is gone (deleted, or an old job from before a restore): no
		// retry will bring it back.
		return river.JobCancel(fmt.Errorf("ingested file %s does not exist: %w", job.Args.FileID, err))
	}
	if err != nil {
		return fmt.Errorf("failed to get ingested file: %w", err)
	}

	// A finished file whose original is gone (KEEP_ORIGINALS=false, or a file
	// deleted by hand) cannot be ingested again. This has to be said before
	// anything is touched: the old chunks are deleted next, and the file would
	// end with no text at all.
	if file.Status == "completed" && !originalExists(file.LocalPath) {
		return river.JobCancel(fmt.Errorf("the original of file %s is not in storage, so it cannot be ingested again; its text and vectors are left as they are", file.ID))
	}

	_, err = w.dbClient.SqlDB.ExecContext(ctx, "DELETE FROM document_chunks WHERE file_id = $1", job.Args.FileID)
	if err != nil {
		return fmt.Errorf("failed to clean up old document chunks: %w", err)
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
		return w.fail(ctx, job, file, fmt.Errorf("failed to open file: %w", err))
	}

	cpDir := ocrCheckpointDir(file.ID)
	text, err := w.manager.Extract(ingest.WithCheckpointDir(ctx, cpDir), file.FileName, f)
	f.Close()
	if err == nil || ingest.IsPermanent(err) || job.Attempt >= job.MaxAttempts {
		// Nothing will resume from the saved pages any more. After any other
		// failure they stay, and the next attempt carries on from them.
		_ = ingest.ClearCheckpoint(cpDir)
	}
	if err != nil {
		return w.fail(ctx, job, file, fmt.Errorf("failed to extract text: %w", err))
	}

	cyrillicCount := 0
	runes := []rune(text)
	for _, r := range text {
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
	if len(chunks) == 0 {
		// Text that is all noise (a table of contents of dots) filters down to
		// nothing; a "completed" file with no chunks cannot be searched.
		return w.fail(ctx, job, file, fmt.Errorf("%w: nothing is left after filtering noise", ingest.ErrNoText))
	}

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
			enrichedBatch = append(enrichedBatch, ingest.EmbeddingText(ingest.ChunkTypeRaw, file.FileName, i+k, chunk))
		}

		vectors, embedErr := w.embedder.EmbedTexts(ctx, enrichedBatch, false)
		if embedErr != nil {
			return w.fail(ctx, job, file, fmt.Errorf("failed to embed batch starting at chunk %d: %w", i, embedErr))
		}

		// Check if vectors match the number of chunks. If not, return an error.
		if len(vectors) != len(enrichedBatch) {
			return w.fail(ctx, job, file, fmt.Errorf("mismatch in vector count vs chunks in batch: expected %d, got %d", len(enrichedBatch), len(vectors)))
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
			chunkTypes = append(chunkTypes, ingest.ChunkTypeRaw)
			metadatas = append(metadatas, "{}")

			if len(vectors[j]) != config.VectorDimension {
				return w.fail(ctx, job, file, fmt.Errorf("invalid vector dimension at chunk %d: %d", i+j, len(vectors[j])))
			}
			embeddings = append(embeddings, pgvector.NewVector(vectors[j]))
		}

		if len(fileIDs) != len(texts) ||
		   len(userIDs) != len(texts) ||
		   len(chunkIndices) != len(texts) ||
		   len(embeddings) != len(texts) ||
		   len(chunkTypes) != len(texts) ||
		   len(metadatas) != len(texts) {
			return w.fail(ctx, job, file, fmt.Errorf(
				"array length mismatch: fileIDs=%d userIDs=%d chunkIndices=%d texts=%d embeddings=%d chunkTypes=%d metadatas=%d",
				len(fileIDs), len(userIDs), len(chunkIndices), len(texts),
				len(embeddings), len(chunkTypes), len(metadatas),
			))
		}

		err = w.dbClient.Queries.CreateDocumentChunksBatch(ctx, db.CreateDocumentChunksBatchParams{
			FileIds:      fileIDs,
			UserIds:      userIDs,
			ChunkIndices: chunkIndices,
			Texts:        texts,
			Embeddings:   embeddings,
			ChunkTypes:   chunkTypes,
			Metadatas:    metadatas,

			EmbeddingModel: config.DefaultEmbedModel,
		})
		if err != nil {
			return w.fail(ctx, job, file, fmt.Errorf("failed to save batch starting at chunk %d: %w", i, err))
		}
	}

	if !file.ManualOverride && w.catMgr != nil {
		decision := w.catMgr.Classify(allVectors)

		err = w.dbClient.Queries.UpdateIngestedFileCategoryAuto(ctx, db.UpdateIngestedFileCategoryAutoParams{
			ID:       file.ID,
			Category: decision.Category,
		})
		if err != nil {
			slog.Warn("Failed to auto-update file category", "file_id", file.ID, "error", err)
		} else {
			slog.Info("Auto-assigned category to file", "file_id", file.ID, "category", decision.Category,
				"leader", decision.Leader, "leader_votes", decision.LeaderVotes, "sampled", decision.Sampled)
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

	// Enqueue distillation worker
	if w.distillation {
		pgxClient, clientErr := river.ClientFromContextSafely[pgx.Tx](ctx)
		if clientErr == nil {
			_, err = pgxClient.Insert(ctx, DistillationWorkerArgs{FileID: file.ID}, distillationInsertOpts())
			if err != nil {
				slog.Error("Failed to enqueue DistillationWorkerArgs", "file_id", file.ID, "error", err)
			}
		} else {
			slog.Warn("Could not enqueue DistillationWorkerArgs: no river pgx client in context")
		}
	}

	w.releaseOriginal(file)

	slog.Info("IngestFileWorker finished successfully", "file_id", job.Args.FileID, "chunks", len(chunks))
	return nil
}

// releaseOriginal decides what happens to the uploaded file once its ingest has
// succeeded. By default it is kept: it is the only copy of the source document
// once that is chunked, and on 2026-10-30 removing it right after ingest left
// nothing to ingest again from when the rows were deleted
// (the 2026-10-30 incident). With KEEP_ORIGINALS=false the owner has
// decided that his own copy of the library is enough, and the file is deleted.
// This is the last step of a successful ingest, so a retry never finds the file
// missing; a failed ingest never gets here and keeps its file.
func (w *IngestFileWorker) releaseOriginal(file db.IngestedFile) {
	if file.LocalPath == "" {
		return
	}
	if w.keepOriginals {
		slog.Info("retained original upload after ingestion", "file_id", file.ID, "path", file.LocalPath)
		return
	}
	if err := os.Remove(file.LocalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("failed to delete the original upload after ingestion", "file_id", file.ID, "path", file.LocalPath, "error", err)
		return
	}
	slog.Info("deleted original upload after ingestion (KEEP_ORIGINALS=false)", "file_id", file.ID, "path", file.LocalPath)
}

// originalExists reports whether the uploaded file of an ingest is still in
// storage. A path that cannot be checked for another reason counts as present:
// the ingest will then report the real error.
func originalExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
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

