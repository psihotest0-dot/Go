-- +goose Up
-- A replaced memory is deleted since 2026-11-14 (processFact), so "superseded"
-- is no longer a status a memory can have. Any row still in it is a fact that
-- was replaced before that change: out of every search, it is deleted here.
DELETE FROM memories WHERE status = 'superseded';
ALTER TABLE memories DROP CONSTRAINT memories_status_check;
ALTER TABLE memories ADD CONSTRAINT memories_status_check CHECK (status IN ('candidate', 'active'));

-- +goose Down
ALTER TABLE memories DROP CONSTRAINT memories_status_check;
ALTER TABLE memories ADD CONSTRAINT memories_status_check CHECK (status IN ('candidate', 'active', 'superseded'));
