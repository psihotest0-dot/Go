-- name: UpsertChatAttachment :one
-- A name that is already attached becomes a new version of the file; the same
-- content again changes nothing but the time. Everything is scoped by user_id.
INSERT INTO chat_attachments (user_id, session_id, name, content, content_hash, bytes, line_count)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, session_id, name) DO UPDATE
SET content      = EXCLUDED.content,
    content_hash = EXCLUDED.content_hash,
    bytes        = EXCLUDED.bytes,
    line_count   = EXCLUDED.line_count,
    version      = CASE WHEN chat_attachments.content_hash <> EXCLUDED.content_hash
                        THEN chat_attachments.version + 1 ELSE chat_attachments.version END,
    updated_at   = now()
RETURNING id, user_id, session_id, name, content_hash, bytes, line_count, version, created_at, updated_at;

-- name: GetChatAttachmentMeta :one
SELECT id, user_id, session_id, name, content_hash, bytes, line_count, version, created_at, updated_at
FROM chat_attachments
WHERE user_id = $1 AND session_id = $2 AND name = $3;

-- name: ListChatAttachments :many
-- Without the content: a list never reads the files.
SELECT id, user_id, session_id, name, content_hash, bytes, line_count, version, created_at, updated_at
FROM chat_attachments
WHERE user_id = $1 AND session_id = $2
ORDER BY updated_at DESC, id;

-- name: ListChatAttachmentsWithContent :many
SELECT * FROM chat_attachments
WHERE user_id = $1 AND session_id = $2
ORDER BY updated_at DESC, id;

-- name: GetChatAttachmentByName :one
SELECT * FROM chat_attachments
WHERE user_id = $1 AND session_id = $2 AND name = $3;

-- name: FindChatAttachmentsByLowerName :many
-- The model may write a name in other letter case than the file has.
SELECT * FROM chat_attachments
WHERE user_id = $1 AND session_id = $2 AND lower(name) = lower($3)
ORDER BY updated_at DESC, id
LIMIT 2;

-- name: SumChatAttachmentBytes :one
SELECT COALESCE(sum(bytes), 0)::bigint AS total_bytes, count(*) AS files
FROM chat_attachments
WHERE user_id = $1 AND session_id = $2;

-- name: DeleteChatAttachment :execrows
DELETE FROM chat_attachments WHERE id = $1 AND user_id = $2;

-- name: DeleteStaleChatAttachments :execrows
-- Conversations are not deleted anywhere, so files of old ones are dropped by age.
DELETE FROM chat_attachments WHERE user_id = $1 AND updated_at < $2;
