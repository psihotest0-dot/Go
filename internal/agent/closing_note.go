package agent

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/db"
)

// The dose of questions at the end of Lili's replies (plan item P49).
//
// The owner wants a few questions, so that a conversation stays alive, not one under every
// reply. The prompt cannot dose it: the rule "end with a statement" loses to the history
// (measured on 2026-11-15 with the real model and the real Telegram history: 11% of the
// replies end with a question without a history, 62% with it, and a looser rule gives two
// thirds, not "a few"). So the code decides, and tells the model by a note next to the
// message it is answering: every fourth reply of a conversation is asked to end with ONE
// question about a detail of the person's words, the others to end with a statement.
// Measured the same way, a note on every turn gave 0 of 18 questions on the statement turns
// and 9 of 9 on the question turns (2026-11-15); with the notes below, 0 of 27 and 27 of 27,
// none with two questions, none that repeated the note, 1 generic (2026-11-16).
//
// CHAT_QUESTION_EVERY sets the distance (4 by default); 0 puts the notes off.
//
// The note is added to the copy of the message that goes to the model. It is never stored:
// a history that carried the notes would teach the model to write them.
const (
	closingNoteStatement = "\n\n[Note for the answer, not from the user: end this reply with a statement (a thought, an observation or an offer), not with a question.]"
	closingNoteQuestion  = "\n\n[Note for the answer, not from the user: end this reply with ONE short question about a specific detail of the person's words. Not a generic question (\"what else interests you?\", \"is there anything else?\", \"how does that sound?\"), not two questions, and not a question right after the person answered one of yours.]"
)

// closingNoteFor is the note for the reply that follows repliesBefore replies of the
// conversation, when every every-th reply ends with a question (the 4th, 8th, 12th... for
// 4; 0 puts the dose off): "" when the dose is off or the reply is technical (code in the
// message, or a file attached to the conversation) and the rule of the prompt, which lets a
// technical answer be as long as it needs, stands alone.
func closingNoteFor(repliesBefore int64, userText string, hasFiles bool, every int) string {
	if every <= 0 || hasFiles || strings.Contains(userText, "```") {
		return ""
	}
	if repliesBefore%int64(every) == int64(every)-1 {
		return closingNoteQuestion
	}
	return closingNoteStatement
}

// withoutClosingNote is the message without the closing note that withClosingNote added to
// it. Whatever another note put after it stays.
func withoutClosingNote(content string) string {
	// The note about pictures (image_note.go) is not the person's either.
	content = strings.Replace(content, "\n\n"+imageNote, "", 1)
	for _, note := range []string{closingNoteQuestion, closingNoteStatement} {
		if i := strings.LastIndex(content, note); i >= 0 {
			return content[:i] + content[i+len(note):]
		}
	}
	return content
}

// withClosingNote puts the note of this turn next to the last message of the history, which
// must be the person's (as it is for a new turn and for a regeneration, which drops the old
// answer first). The count is the replies stored before that message, so a regeneration of
// a reply asks for the same ending as the reply it replaces.
//
// A failure is only logged: the turn goes on without a note, as it did before the note.
func (o *Orchestrator) withClosingNote(ctx context.Context, userID uuid.UUID, sessionID string, c chatContext, hasFiles bool) chatContext {
	last := len(c.History) - 1
	if last < 0 || c.History[last].Role != openai.ChatMessageRoleUser ||
		len(c.HistoryAt) != len(c.History) || c.HistoryAt[last].IsZero() {
		return c
	}
	replies, err := o.db.Queries.CountAssistantRepliesBefore(ctx, db.CountAssistantRepliesBeforeParams{
		UserID:    userID,
		SessionID: sessionID,
		Before:    sql.NullTime{Time: c.HistoryAt[last], Valid: true},
	})
	if err != nil {
		slog.WarnContext(ctx, "could not count the replies of the conversation; no closing note", "err", err, "session_id", sessionID)
		return c
	}
	note := closingNoteFor(replies, c.History[last].Content, hasFiles, o.questionEvery)
	if note == "" {
		return c
	}
	// The history was read for this turn alone and is only ever sent to the model: the
	// stored message stays as the person wrote it.
	c.History[last].Content += note
	return c
}
