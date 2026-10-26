-- +goose Up
ALTER TABLE messages ADD COLUMN analysis_attempts INT NOT NULL DEFAULT 0;

CREATE INDEX idx_messages_stuck
ON messages(user_id, analysis_attempts)
WHERE is_analyzed = false AND analysis_attempts >= 3;

-- +goose Down
DROP INDEX IF EXISTS idx_messages_stuck;
ALTER TABLE messages DROP COLUMN IF EXISTS analysis_attempts;