-- +goose Up
-- +goose StatementBegin
-- The link a document of the library was taken from (2026-11-28). A web page changes between two
-- downloads (its menus, a counter, a token), so the same page added twice - by add_to_library and
-- then by a task over the links - was two files of the same size and another SHA-256: the content
-- hash cannot see it. The key of the link (agent.urlKey: no scheme, no trailing "/", no fragment)
-- is kept with the document, and a link whose key is there is not fetched again. NULL: an upload,
-- and every document stored before.
ALTER TABLE ingested_files ADD COLUMN source_key TEXT;
CREATE INDEX idx_ingested_files_source_key ON ingested_files (user_id, source_key)
    WHERE source_key IS NOT NULL AND deleted_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_ingested_files_source_key;
ALTER TABLE ingested_files DROP COLUMN IF EXISTS source_key;
-- +goose StatementEnd
