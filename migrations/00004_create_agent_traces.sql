-- +goose Up
-- +goose StatementBegin
CREATE TABLE agent_traces (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    session_id TEXT NOT NULL,
    step TEXT NOT NULL,
    details JSONB,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
CREATE INDEX idx_agent_traces_session ON agent_traces(session_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_traces CASCADE;
-- +goose StatementEnd
