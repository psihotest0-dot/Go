-- +goose Up
-- +goose StatementBegin
-- The plan of a task the assistant is working on in a conversation (plan item "task plan",
-- 2026-11-21).
--
-- The history of a conversation keeps the words of the turns, not the results of their tools, so
-- a "continue" started from the model's own earlier words, and that is where it wrote "extractions"
-- of papers it never opened. A plan is kept by the server instead: a goal, steps, and for each
-- step whether it is done, with the evidence that it is. A step is "done" only when the server saw
-- a successful call of the tool the step names (the ledger of the plan holds what the tools did
-- since the plan was made); the model's word for it is not enough. The plan is put in front of the
-- model in every turn of the conversation while it is active.
--
-- steps: [{"id":1,"text":"...","tool":"add_to_library","status":"pending|doing|done|failed|skipped",
-- "evidence":"...","note":"..."}]; ledger: [{"n":1,"tool":"web_fetch","gist":"...","used":true}].
-- At most one plan of a conversation is active; the finished and cancelled ones are kept for a
-- while, to read what was done.
CREATE TABLE task_plans (
    id         BIGSERIAL PRIMARY KEY,
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id TEXT        NOT NULL CHECK (session_id <> ''),
    goal       TEXT        NOT NULL CHECK (goal <> ''),
    steps      JSONB       NOT NULL CHECK (jsonb_typeof(steps) = 'array'),
    ledger     JSONB       NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(ledger) = 'array'),
    status     TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'done', 'cancelled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_task_plans_active ON task_plans (user_id, session_id) WHERE status = 'active';
CREATE INDEX idx_task_plans_session ON task_plans (user_id, session_id, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS task_plans;
-- +goose StatementEnd
