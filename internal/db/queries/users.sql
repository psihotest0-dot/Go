-- name: CreateUser :one
INSERT INTO users DEFAULT VALUES RETURNING *;

-- name: LinkTelegramUser :exec
INSERT INTO telegram_users (telegram_chat_id, user_id)
VALUES ($1, $2)
ON CONFLICT (telegram_chat_id) DO UPDATE SET user_id = EXCLUDED.user_id;

-- name: GetTelegramUser :one
SELECT user_id FROM telegram_users WHERE telegram_chat_id = $1;
