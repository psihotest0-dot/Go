package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"go-monolith/internal/db"
)

// IngestInsertOpts queues a file's ingest on the ingest queue, at most one job
// per file at a time. Without that, a file queued twice (an upload retried, a
// re-ingest started while one is waiting) is extracted, chunked and embedded
// twice, the second run deleting what the first had just written.
func IngestInsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue: "ingest",
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: uniqueWhileInFlightStates(),
		},
	}
}

// ReingestResult says what ReingestFiles did.
type ReingestResult struct {
	Queued     int // jobs inserted
	AlreadyDue int // files that already had an ingest queued or running
	Skipped    int // files that could not be re-ingested (not found, deleted, still being processed)
	// NoOriginal counts finished files whose uploaded original is not in storage
	// (KEEP_ORIGINALS=false, or a file deleted by hand). They are left alone: an
	// ingest would delete their chunks first and then fail to read the file.
	NoOriginal int
}

// ReingestFiles runs the ingest of files again from the originals kept in
// storage, for when the extraction has improved or a model has changed. The
// worker deletes the file's chunks (summaries included) and builds them anew.
//
// ids selects the files; nil selects every file that is finished (completed or
// failed) and not deleted. A file that is still pending or processing is skipped:
// a second run would delete the chunks the first one is writing. A file whose
// original is not in storage is not queued either (see ReingestResult.NoOriginal).
func ReingestFiles(ctx context.Context, database *db.Database, client *river.Client[pgx.Tx], ids []uuid.UUID) (ReingestResult, error) {
	var res ReingestResult

	rows, err := database.SqlDB.QueryContext(ctx, `
		SELECT id, file_name, local_path FROM ingested_files
		WHERE deleted_at IS NULL AND status IN ('completed', 'failed')
		  AND ($1::uuid[] IS NULL OR id = ANY($1::uuid[]))
		ORDER BY created_at`, uuidArray(ids))
	if err != nil {
		return res, fmt.Errorf("failed to list the files: %w", err)
	}
	type file struct {
		id   uuid.UUID
		name string
		path string
	}
	var files []file
	for rows.Next() {
		var f file
		if err := rows.Scan(&f.id, &f.name, &f.path); err != nil {
			rows.Close()
			return res, err
		}
		files = append(files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	if ids != nil {
		res.Skipped = len(ids) - len(files)
	}

	for _, f := range files {
		if !originalExists(f.path) {
			res.NoOriginal++
			slog.Warn("not ingested again: the original is not in storage", "file_id", f.id, "file", f.name)
			continue
		}
		ins, err := client.Insert(ctx, IngestFileArgs{FileID: f.id}, IngestInsertOpts())
		if err != nil {
			return res, fmt.Errorf("failed to queue %s: %w", f.name, err)
		}
		if ins.UniqueSkippedAsDuplicate {
			res.AlreadyDue++
			slog.Info("ingest already queued", "file_id", f.id, "file", f.name)
			continue
		}
		res.Queued++
		slog.Info("ingest queued again", "file_id", f.id, "file", f.name)
	}
	return res, nil
}

// uuidArray passes a nil list as SQL NULL ("every file") and a list as an array.
func uuidArray(ids []uuid.UUID) any {
	if ids == nil {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
