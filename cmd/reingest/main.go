// Command reingest runs the ingest of already uploaded files again, from the
// originals kept in storage\, without uploading them anew. Use it after the text
// extraction has improved (words broken at the end of a line are now joined, for
// instance) or after the embedding model changed.
//
//	go run ./cmd/reingest -list                    # the files and their state
//	go run ./cmd/reingest -file <id>[,<id>...]     # these files
//	go run ./cmd/reingest -all                     # every finished file
//
// The command only queues jobs; the running backend does the work (extract, chunk,
// embed, then distillation). Each file loses its chunks and summaries and gets
// them anew, so run it when the machine can spare the time - a book is about a
// minute of embedding, and its summaries another quarter of an hour of the model.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/jobs"
)

func run() error {
	list := flag.Bool("list", false, "list the files and their state, and exit")
	fileIDs := flag.String("file", "", "comma separated ids of the files to ingest again")
	all := flag.Bool("all", false, "ingest every finished (completed or failed) file again")
	flag.Parse()

	if !*list && !*all && *fileIDs == "" {
		flag.Usage()
		return fmt.Errorf("nothing to do: give -list, -file or -all")
	}
	if *all && *fileIDs != "" {
		return fmt.Errorf("-all and -file exclude each other")
	}

	var ids []uuid.UUID
	if *fileIDs != "" {
		ids = []uuid.UUID{}
		for _, raw := range strings.Split(*fileIDs, ",") {
			id, err := uuid.Parse(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%q is not a file id: %w", raw, err)
			}
			ids = append(ids, id)
		}
	}

	ctx := context.Background()
	database, err := db.Connect(ctx, config.Load().DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()

	if *list {
		return listFiles(ctx, database)
	}

	// An insert-only client: no workers, so nothing is processed here.
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{})
	if err != nil {
		return err
	}
	res, err := jobs.ReingestFiles(ctx, database, client, ids)
	if err != nil {
		return err
	}
	slog.Info("done", "queued", res.Queued, "already_queued", res.AlreadyDue, "skipped", res.Skipped)
	if res.Skipped > 0 {
		slog.Warn("some files were skipped: not found, deleted, or still being processed", "count", res.Skipped)
	}
	return nil
}

func listFiles(ctx context.Context, database *db.Database) error {
	rows, err := database.SqlDB.QueryContext(ctx, `
		SELECT f.id, f.status, left(f.file_name, 60),
		       (SELECT count(*) FROM document_chunks c WHERE c.file_id = f.id AND c.chunk_type = 'raw_text'),
		       (SELECT count(*) FROM document_chunks c WHERE c.file_id = f.id AND c.chunk_type = 'concept_summary')
		FROM ingested_files f WHERE f.deleted_at IS NULL ORDER BY f.created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("%-36s  %-10s  %6s  %9s  %s\n", "id", "status", "chunks", "summaries", "file")
	for rows.Next() {
		var id uuid.UUID
		var status, name string
		var chunks, summaries int
		if err := rows.Scan(&id, &status, &name, &chunks, &summaries); err != nil {
			return err
		}
		fmt.Printf("%-36s  %-10s  %6d  %9d  %s\n", id, status, chunks, summaries, name)
	}
	return rows.Err()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
