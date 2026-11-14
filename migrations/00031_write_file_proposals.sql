-- +goose Up
-- A proposal to write a file (plan item P44): the assistant wants to replace a file, add
-- to one, or - when the turn had read text from outside - create one, and the user
-- confirms it like a rule. The table holds a handful of rows per user: the ALTER is
-- instant.
ALTER TABLE pending_actions DROP CONSTRAINT pending_actions_kind_check;
ALTER TABLE pending_actions ADD CONSTRAINT pending_actions_kind_check CHECK (kind IN ('save_global_rule', 'create_memory', 'save_style_note', 'write_file'));

-- +goose Down
DELETE FROM pending_actions WHERE kind = 'write_file';
ALTER TABLE pending_actions DROP CONSTRAINT pending_actions_kind_check;
ALTER TABLE pending_actions ADD CONSTRAINT pending_actions_kind_check CHECK (kind IN ('save_global_rule', 'create_memory', 'save_style_note'));
