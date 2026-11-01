-- +goose Up
-- +goose StatementBegin
-- The model that produced each chunk's vector. Memories already had the column;
-- chunks did not, so after the embedding service turned out to run a different
-- model than the one recorded everywhere (2026-11-01) there was no way to tell
-- which vectors had to be recomputed. NULL means "not known": the vectors that
-- exist today. cmd/reembed recomputes every row whose model is not the current
-- one, and new rows are written with it (ingest, distillation).
ALTER TABLE document_chunks ADD COLUMN embedding_model TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE document_chunks DROP COLUMN IF EXISTS embedding_model;
-- +goose StatementEnd
