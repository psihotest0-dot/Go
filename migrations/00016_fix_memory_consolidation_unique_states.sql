-- +goose Up
-- +goose StatementBegin
-- River deduplicates a unique job against the state bitmask stored on the
-- *existing* row: river_job_unique_idx matches rows where
-- river_job_state_in_bitmask(unique_states, state) is true. Consolidation jobs
-- enqueued by earlier releases were written with river's default mask, which
-- includes `completed`. Those finished rows therefore stay in the unique index
-- forever, and every later insert with the same args (`{"user_id": ...}`) is
-- silently skipped as a duplicate. That made memory consolidation a one-shot
-- per user: it ran before there was anything worth remembering and never ran
-- again, so memories were never created.
--
-- The enqueue itself now passes an explicit ByState set without `completed`,
-- which fixes every job created from here on. This sweep fixes the rows that
-- are already finished: clearing their mask removes them from the unique index
-- so the next periodic tick can enqueue again. Rows belonging to a job that is
-- still runnable keep their mask on purpose; they are supposed to block.
--
-- river_job is created by rivermigrate, which may run after goose on a fresh
-- database, hence the existence check.
DO $$
BEGIN
    IF to_regclass('public.river_job') IS NOT NULL THEN
        EXECUTE $sql$
            UPDATE river_job
            SET unique_states = NULL
            WHERE kind = 'memory_consolidation'
              AND state = 'completed'
        $sql$;
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Data-only repair: restoring the old masks would restore the bug, so there is
-- nothing to revert here.
SELECT 1;
-- +goose StatementEnd
