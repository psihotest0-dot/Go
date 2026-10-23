-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
    r RECORD;
    fk_name TEXT;
BEGIN
    FOR r IN
        SELECT 'messages' AS t
        UNION ALL SELECT 'memories'
        UNION ALL SELECT 'agent_traces'
        UNION ALL SELECT 'ingested_files'
        UNION ALL SELECT 'document_chunks'
        UNION ALL SELECT 'global_rules'
    LOOP
        -- Find the constraint name for the given table that references the 'users' table
        SELECT con.conname INTO fk_name
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        JOIN pg_class frel ON frel.oid = con.confrelid
        WHERE rel.relname = r.t
          AND frel.relname = 'users'
          AND con.contype = 'f';

        IF fk_name IS NOT NULL THEN
            EXECUTE 'ALTER TABLE ' || r.t || ' DROP CONSTRAINT ' || fk_name;
            EXECUTE 'ALTER TABLE ' || r.t || ' ADD CONSTRAINT ' || fk_name || ' FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE';
        END IF;
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS idx_embedding_cache_created_at ON embedding_cache(created_at);
CREATE INDEX IF NOT EXISTS idx_memories_fact_key ON memories ((metadata->>'fact_key'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_memories_fact_key;
DROP INDEX IF EXISTS idx_embedding_cache_created_at;

DO $$
DECLARE
    r RECORD;
    fk_name TEXT;
BEGIN
    FOR r IN
        SELECT 'messages' AS t
        UNION ALL SELECT 'memories'
        UNION ALL SELECT 'agent_traces'
        UNION ALL SELECT 'ingested_files'
        UNION ALL SELECT 'document_chunks'
        UNION ALL SELECT 'global_rules'
    LOOP
        -- Find the constraint name for the given table that references the 'users' table
        SELECT con.conname INTO fk_name
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        JOIN pg_class frel ON frel.oid = con.confrelid
        WHERE rel.relname = r.t
          AND frel.relname = 'users'
          AND con.contype = 'f';

        IF fk_name IS NOT NULL THEN
            EXECUTE 'ALTER TABLE ' || r.t || ' DROP CONSTRAINT ' || fk_name;
            EXECUTE 'ALTER TABLE ' || r.t || ' ADD CONSTRAINT ' || fk_name || ' FOREIGN KEY (user_id) REFERENCES users(id)';
        END IF;
    END LOOP;
END $$;
-- +goose StatementEnd
