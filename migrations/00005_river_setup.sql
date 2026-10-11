-- +goose Up
-- +goose StatementBegin
-- River migrations are handled via the river CLI, but we can set up the tables
CREATE TABLE IF NOT EXISTS river_migration (
    id bigserial PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    version bigint NOT NULL,
    version_line varchar(255) NOT NULL
);

CREATE TABLE IF NOT EXISTS river_job (
    id bigserial PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    state varchar(255) NOT NULL DEFAULT 'available',
    attempt integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL,
    attempted_at timestamptz,
    finalized_at timestamptz,
    queue varchar(255) NOT NULL DEFAULT 'default',
    priority integer NOT NULL DEFAULT 1,
    args jsonb NOT NULL,
    attempted_by jsonb,
    errors jsonb,
    tags varchar(255)[] NOT NULL DEFAULT '{}',
    scheduled_at timestamptz NOT NULL DEFAULT now(),
    kind varchar(255) NOT NULL,
    unique_key bytea,
    unique_states bit(8)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS river_job CASCADE;
DROP TABLE IF EXISTS river_migration CASCADE;
-- +goose StatementEnd
