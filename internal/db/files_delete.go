package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// DeleteFile removes a file of a user from the library, in one transaction: the
// row is marked deleted (and kept, as a record of what was there) and its chunks,
// the raw text and the concept summaries with their vectors, are deleted. It
// returns how many chunks went.
//
// The chunks used to be kept so that a delete could be undone, but nothing ever
// undid one, and they stayed in the vector indexes, where they took places among
// the nearest candidates of every search (2026-11-14: 17 853 chunks of 7 deleted
// files, 11 % of the table). A deleted file comes back by uploading it again; the
// database backups (go-backup-1, every 6 hours) hold the rest.
//
// Only this file's chunks are touched: the 2026-10-30 incident was a cascade from
// users that took the whole library, which a trigger now forbids.
//
// The original upload is not touched here; the caller removes it after the commit.
func (d *Database) DeleteFile(ctx context.Context, userID, fileID uuid.UUID) (int64, error) {
	tx, err := d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	q := d.Queries.WithTx(tx)

	// The row first: its lock makes a concurrent chunk insert (CreateDocumentChunksBatch,
	// the distillation) wait, and then see the file as deleted.
	if err := q.SoftDeleteIngestedFile(ctx, SoftDeleteIngestedFileParams{ID: fileID, UserID: userID}); err != nil {
		return 0, fmt.Errorf("mark deleted: %w", err)
	}
	chunks, err := q.DeleteFileChunks(ctx, DeleteFileChunksParams{FileID: fileID, UserID: userID})
	if err != nil {
		return 0, fmt.Errorf("delete chunks: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return chunks, nil
}
