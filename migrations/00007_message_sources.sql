-- +goose Up
-- +goose StatementBegin
ALTER TABLE messages ADD COLUMN source_chunk_ids JSONB DEFAULT '[]'::jsonb;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE messages DROP COLUMN source_chunk_ids;
-- +goose StatementEnd
