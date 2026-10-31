-- +goose Up
-- +goose StatementBegin
-- Every table that stores user data cascades from users (messages, memories,
-- agent_traces, ingested_files, document_chunks, global_rules, ...), so one
-- `DELETE FROM users` empties the whole library. That is what happened on
-- 2026-10-30 (docs/incident-2026-10-30/REPORT.md). The guard below turns such
-- a statement into an error unless the transaction opts in explicitly.
CREATE OR REPLACE FUNCTION prevent_user_delete() RETURNS trigger AS $$
BEGIN
    IF current_setting('app.allow_user_delete', true) = 'on' THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NULL;
    END IF;

    RAISE EXCEPTION 'refusing to % users: every table with user data cascades from users. To do it on purpose, run SET LOCAL app.allow_user_delete = ''on'' inside the transaction first (docs/incident-2026-10-30/REPORT.md)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER users_prevent_delete
    BEFORE DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION prevent_user_delete();

-- TRUNCATE does not fire row-level triggers and wipes the same data.
CREATE TRIGGER users_prevent_truncate
    BEFORE TRUNCATE ON users
    FOR EACH STATEMENT EXECUTE FUNCTION prevent_user_delete();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS users_prevent_truncate ON users;
DROP TRIGGER IF EXISTS users_prevent_delete ON users;
DROP FUNCTION IF EXISTS prevent_user_delete();
-- +goose StatementEnd
