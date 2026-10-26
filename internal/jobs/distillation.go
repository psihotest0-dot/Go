package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
)

type DistillationWorkerArgs struct {
	FileID uuid.UUID `json:"file_id"`
}

func (DistillationWorkerArgs) Kind() string {
	return "distillation_worker"
}

type DistillationWorker struct {
	river.WorkerDefaults[DistillationWorkerArgs]
	dbClient  *db.Database
	llmClient llm.LLMClient
	embedder  llm.Embedder
}

func NewDistillationWorker(dbClient *db.Database, llmClient llm.LLMClient, embedder llm.Embedder) *DistillationWorker {
	return &DistillationWorker{
		dbClient:  dbClient,
		llmClient: llmClient,
		embedder:  embedder,
	}
}

type DistillationSchema struct {
	Summary string   `json:"summary"`
	Themes  []string `json:"themes"`
}

func (w *DistillationWorker) Timeout(job *river.Job[DistillationWorkerArgs]) time.Duration {
	return 90 * time.Minute // Large documents may take time
}

func (w *DistillationWorker) Work(ctx context.Context, job *river.Job[DistillationWorkerArgs]) error {
	slog.Info("DistillationWorker started", "file_id", job.Args.FileID)

	if !config.Load().EnableDistillation {
		slog.Info("Distillation skipped because EnableDistillation is false", "file_id", job.Args.FileID)
		return nil
	}

	// Fetch file to ensure it's still there and completed
	file, err := w.dbClient.Queries.GetIngestedFileByID(ctx, job.Args.FileID)
	if err != nil {
		return fmt.Errorf("failed to get ingested file: %w", err)
	}

	// Fetch all raw_text chunks
	chunks, err := w.dbClient.Queries.GetDocumentChunksByFile(ctx, db.GetDocumentChunksByFileParams{
		FileID: job.Args.FileID,
		UserID: file.UserID,
	})
	if err != nil {
		return fmt.Errorf("failed to get document chunks: %w", err)
	}

	var rawChunks []db.DocumentChunk
	var conceptChunks []db.DocumentChunk
	for _, c := range chunks {
		switch c.ChunkType {
		case "raw_text":
			rawChunks = append(rawChunks, c)
		case "concept_summary":
			conceptChunks = append(conceptChunks, c)
		}
	}

	if len(rawChunks) == 0 {
		return nil
	}

	batchSize := 5
	expectedConcepts := (len(rawChunks) + batchSize - 1) / batchSize

	if float64(len(conceptChunks)) >= float64(expectedConcepts)*0.8 {
		slog.Info("Distillation skipped (sufficient coverage)",
			"file_id", job.Args.FileID,
			"have", len(conceptChunks),
			"expected", expectedConcepts)
		return nil
	}

	if len(conceptChunks) > 0 {
		slog.Info("Re-distilling (partial coverage)",
			"file_id", job.Args.FileID,
			"have", len(conceptChunks),
			"expected", expectedConcepts)
		_, err = w.dbClient.SqlDB.ExecContext(ctx,
			`DELETE FROM document_chunks WHERE file_id = $1 AND chunk_type = 'concept_summary'`,
			file.ID)
		if err != nil {
			return fmt.Errorf("failed to clear old concept chunks: %w", err)
		}
	}

	// Process in batches (sliding windows of 5 chunks)
	var maxChunkIndex int
	err = w.dbClient.SqlDB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(chunk_index), -1) FROM document_chunks WHERE file_id = $1`,
		file.ID).Scan(&maxChunkIndex)
	if err != nil {
		return fmt.Errorf("failed to get max chunk index: %w", err)
	}
	nextConceptIndex := maxChunkIndex + 1
	successCount := 0
	failCount := 0

	for i := 0; i < len(rawChunks); i += batchSize {
		end := i + batchSize
		if end > len(rawChunks) {
			end = len(rawChunks)
		}

		batch := rawChunks[i:end]
		var textContent string
		var sourceChunkIDs []uuid.UUID
		for _, c := range batch {
			textContent += c.Text + "\n\n"
			sourceChunkIDs = append(sourceChunkIDs, c.ID)
		}

		systemPrompt := "You are an expert at extracting core concepts, themes, and summaries from text. Extract the main thesis, themes, and write a high-level summary of the provided text."

		req := openai.ChatCompletionRequest{
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
				{Role: openai.ChatMessageRoleUser, Content: textContent},
			},
		}

		resp, err := w.llmClient.ChatWithJSONSchema(ctx, req, DistillationSchema{})
		if err != nil {
			slog.Error("failed to distill batch", "batch_start", i, "error", err)
			failCount++
			continue
		}

		if len(resp.Choices) > 0 {
			var result DistillationSchema
			content := resp.Choices[0].Message.Content
			// Clean up potential markdown formatting
			if len(content) > 7 && content[:7] == "```json" {
				content = content[7:]
				if len(content) > 3 && content[len(content)-3:] == "```" {
					content = content[:len(content)-3]
				}
			}

			err = json.Unmarshal([]byte(content), &result)
			if err != nil {
				slog.Error("failed to parse distillation response", "batch_start", i, "error", err, "content", content)
				failCount++
				continue
			}

			// Embed the summary
			vectors, err := w.embedder.EmbedTexts(ctx, []string{result.Summary}, false)
			if err != nil || len(vectors) == 0 || len(vectors[0]) != config.VectorDimension {
				slog.Error("failed to embed distillation summary", "batch_start", i, "error", err)
				failCount++
				continue
			}
			embedding := pgvector.NewVector(vectors[0])

			// Create metadata
			metaMap := map[string]interface{}{
				"source_chunk_ids": sourceChunkIDs,
				"themes":           result.Themes,
			}
			metaBytes, err := json.Marshal(metaMap)
			if err != nil {
				slog.Error("failed to marshal metadata", "batch_start", i, "error", err)
				failCount++
				continue
			}

			// Store it directly
			_, err = w.dbClient.SqlDB.ExecContext(ctx,
				`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				file.ID, file.UserID, nextConceptIndex, result.Summary, embedding, "concept_summary", metaBytes,
			)
			if err != nil {
				slog.Error("failed to insert concept_summary",
					"chunk_index", nextConceptIndex, "error", err)
				return fmt.Errorf("failed to insert concept_summary at index %d: %w", nextConceptIndex, err)
			}
			nextConceptIndex++
			successCount++
		}
	}

	slog.Info("DistillationWorker finished",
		"file_id", job.Args.FileID,
		"success", successCount,
		"failed", failCount,
		"concepts_created", successCount)

	if successCount == 0 && failCount > 0 {
		return fmt.Errorf("all %d distillation batches failed for file %s", failCount, file.ID)
	}

	if failCount > 0 {
		slog.Warn("Distillation completed with partial failures",
			"file_id", job.Args.FileID,
			"success", successCount, "failed", failCount)
	}

	return nil
}
