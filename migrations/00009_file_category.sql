-- +goose Up
-- +goose StatementBegin
ALTER TABLE ingested_files ADD COLUMN category TEXT NOT NULL DEFAULT 'general';
ALTER TABLE ingested_files ADD COLUMN manual_override BOOLEAN NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE ingested_files DROP COLUMN category;
ALTER TABLE ingested_files DROP COLUMN manual_override;
-- +goose StatementEnd
