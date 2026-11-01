// Command recategorize works out the category of the files again from the vectors
// that are already stored, without ingesting anything. Run it after the way the
// category is chosen has changed (retrieval.CategoryManager.Classify), or after the
// example sentences of the categories have.
//
//	go run ./cmd/recategorize           # only shows what would change
//	go run ./cmd/recategorize -apply    # writes the new categories
//
// A file whose category was set by hand (PATCH /v1/files/{id}/category) is left
// alone. The backend does not have to be stopped.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

// sampleChunks is how many chunks from the start of a file are read; Classify
// uses the first ones and ignores the rest.
const sampleChunks = 30

type fileRow struct {
	id       uuid.UUID
	name     string
	category string
}

func run() error {
	apply := flag.Bool("apply", false, "write the new categories (without it only the differences are shown)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := config.Load()
	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer database.Close()

	// The centroids of the categories are made by the embedding service, so it has
	// to be the right one.
	embedder := llm.NewHTTPEmbedder(database).WithTimeout(cfg.EmbedderTimeout)
	if err := embedder.CheckModel(ctx); err != nil {
		return err
	}
	catMgr, err := retrieval.NewCategoryManager(ctx, embedder)
	if err != nil {
		return err
	}

	rows, err := database.SqlDB.QueryContext(ctx, `
		SELECT id, file_name, category FROM ingested_files
		WHERE deleted_at IS NULL AND status = 'completed' AND NOT manual_override
		ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("failed to list the files: %w", err)
	}
	var files []fileRow
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.id, &f.name, &f.category); err != nil {
			rows.Close()
			return err
		}
		files = append(files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	changed := 0
	for _, f := range files {
		vectors, err := firstVectors(ctx, database, f.id)
		if err != nil {
			return fmt.Errorf("failed to read the vectors of %s: %w", f.name, err)
		}
		decision := catMgr.Classify(vectors)
		mark := "same   "
		if decision.Category != f.category {
			mark = "CHANGED"
			changed++
		}
		fmt.Printf("%s  %-11s -> %-11s (leader %s %d of %d)  %s\n", mark, f.category, decision.Category,
			decision.Leader, decision.LeaderVotes, decision.Sampled, f.name)

		if *apply && decision.Category != f.category {
			if err := database.Queries.UpdateIngestedFileCategoryAuto(ctx, db.UpdateIngestedFileCategoryAutoParams{
				ID: f.id, Category: decision.Category,
			}); err != nil {
				return fmt.Errorf("failed to update %s: %w", f.name, err)
			}
		}
	}

	switch {
	case changed == 0:
		fmt.Println("nothing to change")
	case *apply:
		fmt.Printf("%d categories written\n", changed)
	default:
		fmt.Printf("%d categories would change; run again with -apply to write them\n", changed)
	}
	return nil
}

// firstVectors reads the vectors of the first chunks of a file, in reading order.
func firstVectors(ctx context.Context, database *db.Database, fileID uuid.UUID) ([][]float32, error) {
	rows, err := database.SqlDB.QueryContext(ctx, `
		SELECT embedding FROM document_chunks
		WHERE file_id = $1 AND chunk_type = 'raw_text'
		ORDER BY chunk_index LIMIT $2`, fileID, sampleChunks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][]float32
	for rows.Next() {
		var v pgvector.Vector
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v.Slice())
	}
	return out, rows.Err()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
