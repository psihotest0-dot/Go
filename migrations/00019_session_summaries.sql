-- +goose Up
-- +goose StatementBegin
-- The rolling summary of a chat session: a short digest of the messages that no
-- longer fit into the prompt. It used to live only in the process memory, so a
-- restart threw it away and the next turn started again from nothing.
--
-- covered_until_at is the position: every message of the session created at or
-- before it is part of the summary. It is a timestamp, not a message reference,
-- because messages are not stable (a regenerated answer replaces the last one)
-- and a dangling reference would make the position unreadable.
-- covered_until_message_id names the last summarised message for diagnostics
-- only, so it deliberately has no foreign key.
CREATE TABLE session_summaries (
    user_id                  UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id               TEXT        NOT NULL,
    summary                  TEXT        NOT NULL,
    covered_until_message_id UUID        NOT NULL,
    covered_until_at         TIMESTAMPTZ NOT NULL,
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, session_id)
);

-- Finds the messages that still have to be summarised: one session, in order,
-- starting after the covered position.
CREATE INDEX IF NOT EXISTS idx_messages_user_session_created
    ON messages (user_id, session_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_user_session_created;
DROP TABLE IF EXISTS session_summaries;
-- +goose StatementEnd
