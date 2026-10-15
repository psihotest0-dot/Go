-- +goose Up
-- +goose StatementBegin
ALTER TABLE messages ADD COLUMN is_analyzed BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX idx_messages_is_analyzed ON messages(is_analyzed);

-- Backfill: mark messages as analyzed if they already have an associated memory
UPDATE messages SET is_analyzed = TRUE WHERE id IN (SELECT source_message_id FROM memories WHERE source_message_id IS NOT NULL);

-- Cleanup old dummy memories
DELETE FROM memories WHERE content = '[analyzed]' AND status = 'superseded';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_is_analyzed;
ALTER TABLE messages DROP COLUMN is_analyzed;
-- +goose StatementEnd
