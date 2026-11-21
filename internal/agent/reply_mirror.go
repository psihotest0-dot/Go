package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"go-monolith/internal/api"
)

// mirrorTimeout bounds the repeat of one answer in Telegram. It runs while the conversation is
// still held (see mirrorReply), so a slow Telegram must not hold it for long.
const mirrorTimeout = 10 * time.Second

// ReplyMirror repeats an answer that was given in the app in the other channel of a shared
// conversation: the Telegram bot sends the question and the answer to the chat. question is what
// the person wrote in the app; regenerated says that the answer is a new one for the last
// question, not an answer to a message just sent. It must not panic or block for long.
type ReplyMirror func(ctx context.Context, userID uuid.UUID, sessionID, question, answer string, regenerated bool)

// SetReplyMirror sets who repeats the answers given in the app to a conversation shared with
// Telegram (a session "tg-<chat id>"); nil stops it. The Telegram bot sets itself when it has
// connected and clears itself when it stops.
func (o *Orchestrator) SetReplyMirror(m ReplyMirror) {
	if m == nil {
		o.mirror.Store(nil)
		return
	}
	o.mirror.Store(&m)
}

// mirrorReply tells the mirror about an answer that has just been given in the app. It is for the
// app's own turns only: the answers of the Telegram bot go to the chat by themselves
// (ProcessChatTurn does not call it). Only a conversation shared with Telegram is repeated, and only
// when the answer is something to read (a model that only "thought" gave nothing).
//
// It is called before the conversation is let go, so that the repeat is in the chat before the
// answer to a message that waited behind this turn; the stream has already ended for the app, which
// does not wait for it. A panic of the mirror is logged and goes no further: the answer is given
// and stored whatever happens here.
func (o *Orchestrator) mirrorReply(ctx context.Context, userID uuid.UUID, sessionID, question, answer string, regenerated bool) {
	m := o.mirror.Load()
	if m == nil || !api.IsTelegramSession(sessionID) {
		return
	}
	answer = sanitizeAssistantText(answer)
	if answer == "" {
		return
	}

	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mirrorTimeout)
	defer cancel()
	defer func() {
		if rcv := recover(); rcv != nil {
			slog.ErrorContext(ctx, "panic while repeating an answer in Telegram", "panic", rcv)
		}
	}()
	(*m)(mctx, userID, sessionID, question, answer, regenerated)
}
