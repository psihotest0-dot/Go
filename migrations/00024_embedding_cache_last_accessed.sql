-- +goose Up
-- +goose StatementBegin
-- The cache is cleaned by age (30 days). Age since creation threw away vectors
-- that were still in use: a re-ingest of a book older than a month embedded every
-- chunk again (about 85 minutes for the whole library). The cleaning now goes by
-- the last use, which EmbedTexts renews (at most once a day per entry).
ALTER TABLE embedding_cache ADD COLUMN last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT now();

UPDATE embedding_cache SET last_accessed_at = COALESCE(created_at, now());

CREATE INDEX idx_embedding_cache_last_accessed_at ON embedding_cache (last_accessed_at);
DROP INDEX IF EXISTS idx_embedding_cache_created_at;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_embedding_cache_created_at ON embedding_cache (created_at);
DROP INDEX IF EXISTS idx_embedding_cache_last_accessed_at;
ALTER TABLE embedding_cache DROP COLUMN IF EXISTS last_accessed_at;
-- +goose StatementEnd
