-- +goose Up
-- +goose StatementBegin
-- A plan of a task that the server walks itself (plan steps 3.3-3.7, 2026-11-23).
--
-- The plan of 2026-11-21 was the model's: it made the steps and did them with its tools, and the
-- replay on the real model showed that a 12B model does not walk 13 sources that way (5 files, none
-- from its source). Here the code makes the steps (one per document), asks the person once before
-- it starts, and walks them in the background: for each document the server reads its passages,
-- the model writes one short file from them, and the code checks the numbers before the file is
-- written.
--
-- kind: '' is a plan of the model (as before); any other value is the recipe the server walks
-- ('key_facts', 'summary'). proposed: the server has made the plan and waits for the person's yes;
-- it is not run and not shown to the model. One proposed and one active plan of each kind per
-- conversation at a time.
--
-- The table holds a few rows per conversation; the constraint and the indexes are rebuilt at once.
ALTER TABLE task_plans DROP CONSTRAINT task_plans_status_check;
ALTER TABLE task_plans ADD CONSTRAINT task_plans_status_check
    CHECK (status IN ('proposed', 'active', 'done', 'cancelled'));
ALTER TABLE task_plans ADD COLUMN kind TEXT NOT NULL DEFAULT '';

DROP INDEX idx_task_plans_active;
CREATE UNIQUE INDEX idx_task_plans_active ON task_plans (user_id, session_id, kind) WHERE status = 'active';
CREATE UNIQUE INDEX idx_task_plans_proposed ON task_plans (user_id, session_id) WHERE status = 'proposed';
-- The plans the server is walking, read at the start and after every step.
CREATE INDEX idx_task_plans_running ON task_plans (id) WHERE status = 'active' AND kind <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM task_plans WHERE kind <> '' OR status = 'proposed';
DROP INDEX IF EXISTS idx_task_plans_running;
DROP INDEX IF EXISTS idx_task_plans_proposed;
DROP INDEX idx_task_plans_active;
CREATE UNIQUE INDEX idx_task_plans_active ON task_plans (user_id, session_id) WHERE status = 'active';
ALTER TABLE task_plans DROP COLUMN kind;
ALTER TABLE task_plans DROP CONSTRAINT task_plans_status_check;
ALTER TABLE task_plans ADD CONSTRAINT task_plans_status_check
    CHECK (status IN ('active', 'done', 'cancelled'));
-- +goose StatementEnd
