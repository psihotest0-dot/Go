-- +goose Up
-- +goose StatementBegin
CREATE INDEX idx_document_chunks_embedding ON document_chunks USING hnsw (embedding vector_cosine_ops);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_document_chunks_embedding;
-- +goose StatementEnd
