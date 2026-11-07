-- +goose Up
-- +goose StatementBegin
-- Text files the user attaches to ONE conversation (code, logs, a long paste turned
-- into a file). They are not the library: nothing is split into passages, embedded or
-- summarised, and the other chats never see them. The chat prompt shows the ones
-- that fit and the model reads the rest through the read_attachment tool.
--
-- A name is one file per session: attaching the same name again is a new version of
-- the file (content_hash tells whether it really changed), which is what happens
-- when someone edits a script and sends it again.
CREATE TABLE chat_attachments (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id   TEXT        NOT NULL CHECK (char_length(session_id) BETWEEN 1 AND 128),
    name         TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 255),
    content      TEXT        NOT NULL,
    -- sha256 of the content, hex.
    content_hash TEXT        NOT NULL CHECK (char_length(content_hash) = 64),
    -- Size of the content in bytes (UTF-8) and the number of lines, kept so a list
    -- never has to read the content.
    bytes        INTEGER     NOT NULL CHECK (bytes >= 0),
    line_count   INTEGER     NOT NULL CHECK (line_count >= 0),
    version      INTEGER     NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, session_id, name)
);

-- The attachments of one session, newest first; the unique constraint above already
-- serves the lookup by name.
CREATE INDEX idx_chat_attachments_session
    ON chat_attachments (user_id, session_id, updated_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS chat_attachments;
-- +goose StatementEnd
