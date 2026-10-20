-- +goose Up
CREATE TABLE telegram_users (
    telegram_chat_id BIGINT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE telegram_users;
