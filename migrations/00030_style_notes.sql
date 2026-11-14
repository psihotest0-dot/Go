-- +goose Up
-- Notes on how to talk with the user (plan item P42): they live beside the global
-- rules - confirmed by the user, always in the prompt, listed and deleted the same
-- way - but the prompt shows them in a section of their own ("Как говорим ние
-- двамата"), as observations of the person rather than instructions. A note and a
-- rule may share a key, so the key is unique per kind. Both tables hold a handful of
-- rows per user: the ALTERs are instant.
ALTER TABLE global_rules ADD COLUMN kind TEXT NOT NULL DEFAULT 'rule' CHECK (kind IN ('rule', 'style'));
ALTER TABLE global_rules DROP CONSTRAINT global_rules_user_id_key_key;
ALTER TABLE global_rules ADD CONSTRAINT global_rules_user_id_kind_key_key UNIQUE (user_id, kind, key);
ALTER TABLE pending_actions DROP CONSTRAINT pending_actions_kind_check;
ALTER TABLE pending_actions ADD CONSTRAINT pending_actions_kind_check CHECK (kind IN ('save_global_rule', 'create_memory', 'save_style_note'));

-- +goose Down
DELETE FROM pending_actions WHERE kind = 'save_style_note';
ALTER TABLE pending_actions DROP CONSTRAINT pending_actions_kind_check;
ALTER TABLE pending_actions ADD CONSTRAINT pending_actions_kind_check CHECK (kind IN ('save_global_rule', 'create_memory'));
DELETE FROM global_rules WHERE kind = 'style';
ALTER TABLE global_rules DROP CONSTRAINT global_rules_user_id_kind_key_key;
ALTER TABLE global_rules ADD CONSTRAINT global_rules_user_id_key_key UNIQUE (user_id, key);
ALTER TABLE global_rules DROP COLUMN kind;
