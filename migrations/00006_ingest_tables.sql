-- +goose Up
-- +goose StatementBegin
CREATE TABLE ingested_files (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    file_name TEXT NOT NULL,
    file_hash TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    local_path TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
CREATE INDEX idx_ingested_files_user_id ON ingested_files(user_id);
CREATE UNIQUE INDEX idx_ingested_files_user_hash ON ingested_files(user_id, file_hash);

CREATE TABLE document_chunks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id UUID NOT NULL REFERENCES ingested_files(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id),
    chunk_index INTEGER NOT NULL,
    text TEXT NOT NULL,
    embedding vector(384) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
CREATE INDEX idx_document_chunks_file_id ON document_chunks(file_id);
CREATE INDEX idx_document_chunks_user_id ON document_chunks(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE document_chunks;
DROP TABLE ingested_files;
-- +goose StatementEnd
