-- +goose Up
-- +goose StatementBegin
-- Soft delete for ingested files.
--
-- A hard DELETE on ingested_files cascades into document_chunks, so one wrong
-- delete (or a cascade from users) permanently discarded the extracted text and
-- embeddings of the whole library on 2026-10-30. Keeping the row and the chunks
-- around lets a mistaken delete be undone with a single UPDATE.
ALTER TABLE ingested_files ADD COLUMN deleted_at TIMESTAMP WITH TIME ZONE;

-- The deduplication index must only consider live rows, otherwise a re-upload of
-- a previously deleted document would be rejected as a duplicate.
DROP INDEX IF EXISTS idx_ingested_files_user_hash;
CREATE UNIQUE INDEX idx_ingested_files_user_hash
    ON ingested_files(user_id, file_hash)
    WHERE deleted_at IS NULL;

-- Listing and search join on user_id, so the partial index keeps those scans on
-- the live rows only.
CREATE INDEX idx_ingested_files_user_id_active
    ON ingested_files(user_id)
    WHERE deleted_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_ingested_files_user_id_active;
DROP INDEX IF EXISTS idx_ingested_files_user_hash;

-- Soft deleted rows cannot satisfy the unconditional unique index that is
-- restored below, so they are removed first. This is why the down migration is
-- destructive: run it only on a database whose soft deleted files no longer
-- matter.
DELETE FROM ingested_files WHERE deleted_at IS NOT NULL;
CREATE UNIQUE INDEX idx_ingested_files_user_hash ON ingested_files(user_id, file_hash);
ALTER TABLE ingested_files DROP COLUMN IF EXISTS deleted_at;
-- +goose StatementEnd
