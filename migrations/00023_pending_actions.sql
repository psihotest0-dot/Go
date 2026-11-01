-- +goose Up
-- +goose StatementBegin
-- Writes the model asks for that outlive the conversation (a global rule, a
-- memory) are proposals the user confirms. A rule is the first thing in the
-- system prompt of every session, so text from a web page or a book must not be
-- able to plant one by itself.
CREATE TABLE pending_actions (
    id               BIGSERIAL PRIMARY KEY,
    user_id          UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id       TEXT        NOT NULL DEFAULT '',
    kind             TEXT        NOT NULL CHECK (kind IN ('save_global_rule', 'create_memory')),
    -- The arguments of the tool call, already validated.
    payload          JSONB       NOT NULL,
    -- Hash of kind + payload: the same proposal made twice is one row.
    payload_hash     TEXT        NOT NULL,
    -- Where untrusted text came from in the turn that made the proposal
    -- (web_search, web_fetch, search_documents, retrieved_documents). Empty
    -- means the turn read nothing but the conversation.
    external_sources TEXT[]      NOT NULL DEFAULT '{}',
    status           TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at       TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_pending_actions_open_proposal
    ON pending_actions (user_id, payload_hash)
    WHERE status = 'pending';

CREATE INDEX idx_pending_actions_user_status
    ON pending_actions (user_id, status, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pending_actions;
-- +goose StatementEnd
