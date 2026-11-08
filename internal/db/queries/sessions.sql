-- Deleting a chat session (DELETE /chat/sessions/{id}). The statements run in one
-- transaction. The memories derived from the session are NOT touched:
-- memories.source_message_id is ON DELETE SET NULL, so they only lose the link.

-- name: DeleteSessionAttachments :execrows
DELETE FROM chat_attachments WHERE user_id = $1 AND session_id = $2;

-- name: DeleteSessionTraces :execrows
DELETE FROM agent_traces WHERE user_id = $1 AND session_id = $2;

-- name: DeleteSessionSummary :execrows
DELETE FROM session_summaries WHERE user_id = $1 AND session_id = $2;

-- A user message is read for memories by a background job (every 5 minutes). One the job
-- has not read yet (not analysed, fewer than 3 failed attempts, with text) is not deleted
-- here: RetireSessionMessages moves it out of the session and PurgeRetiredMessages removes it
-- as soon as it has been read, so a fact told shortly before the chat was deleted still
-- becomes a memory. The worker marks every other message as read without looking at it.
-- name: DeleteSessionMessages :execrows
DELETE FROM messages
WHERE user_id = $1 AND session_id = $2
  AND NOT (role = 'user' AND NOT is_analyzed AND analysis_attempts < 3 AND btrim(content) <> '');

-- name: RetireSessionMessages :execrows
UPDATE messages SET session_id = sqlc.arg('retired_session_id')
WHERE user_id = sqlc.arg('user_id') AND session_id = sqlc.arg('session_id');

-- Run by the periodic memory job. The prefix is the one RetiredSessionPrefix names in Go.
-- name: PurgeRetiredMessages :execrows
DELETE FROM messages WHERE is_analyzed AND session_id LIKE 'deleted:%';
