-- +goose Up
-- +goose StatementBegin
CREATE INDEX idx_memories_core ON memories(user_id, importance DESC, updated_at DESC);
ALTER TABLE document_chunks ADD COLUMN chunk_type TEXT NOT NULL DEFAULT 'raw_text';
ALTER TABLE document_chunks ADD COLUMN metadata JSONB NOT NULL DEFAULT '{}'::jsonb;
CREATE INDEX idx_doc_chunks_type ON document_chunks(chunk_type);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_doc_chunks_type;
ALTER TABLE document_chunks DROP COLUMN IF EXISTS chunk_type;
ALTER TABLE document_chunks DROP COLUMN IF EXISTS metadata;
DROP INDEX IF EXISTS idx_memories_core;
-- +goose StatementEnd
