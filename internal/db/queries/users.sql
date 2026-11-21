-- name: CreateUser :one
INSERT INTO users DEFAULT VALUES RETURNING *;

-- name: LinkTelegramUser :exec
INSERT INTO telegram_users (telegram_chat_id, user_id)
VALUES ($1, $2)
ON CONFLICT (telegram_chat_id) DO UPDATE SET user_id = EXCLUDED.user_id;

-- name: GetTelegramUser :one
SELECT user_id FROM telegram_users WHERE telegram_chat_id = $1;

-- The Telegram chats tied to a person (/link), lowest chat id first: each is a conversation the
-- app can open (session tg-<chat id>).
-- name: ListTelegramChatsByUser :many
SELECT telegram_chat_id FROM telegram_users WHERE user_id = $1 ORDER BY telegram_chat_id;
