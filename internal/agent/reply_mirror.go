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

// OutsideNotifier sends a text the server wrote on its own in a conversation shared with
// Telegram (the report of a task it walked in the background, package taskrun) to that chat. It
// must not panic or block for long.
type OutsideNotifier func(ctx context.Context, userID uuid.UUID, sessionID, text string)

// SetOutsideNotifier sets who sends such texts; nil stops it. The Telegram bot sets itself as it
// does for the mirror. The texts that came while nobody was set go out now (outside_queue.go).
func (o *Orchestrator) SetOutsideNotifier(n OutsideNotifier) {
	if n == nil {
		o.outside.Store(nil)
		return
	}
	o.outside.Store(&n)
	o.sendHeldOutside(n)
}

// NotifyOutside sends text to the other channel of a conversation shared with Telegram (a session
// "tg-<chat id>"); for any other conversation it does nothing - the text is in the conversation
// already. With nobody set (the bot has not connected yet, or is connecting again) the text waits
// for the bot (outside_queue.go). It is the notifier of the reminders, the monthly report of the
// bills and the runner of tasks (main.go).
func (o *Orchestrator) NotifyOutside(ctx context.Context, userID uuid.UUID, sessionID, text string) {
	if !api.IsTelegramSession(sessionID) || text == "" {
		return
	}
	var n OutsideNotifier
	if p := o.outside.Load(); p != nil {
		n = *p
	} else if held, ok := o.holdOutside(userID, sessionID, text); ok {
		n = held // set meanwhile
	} else {
		return
	}
	o.sendOutside(ctx, n, userID, sessionID, text)
}

// sendOutside hands one text to n, with a bound on the time and a guard against its panic.
func (o *Orchestrator) sendOutside(ctx context.Context, n OutsideNotifier, userID uuid.UUID, sessionID, text string) {
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mirrorTimeout)
	defer cancel()
	defer func() {
		if rcv := recover(); rcv != nil {
			slog.ErrorContext(ctx, "panic while sending a text of the server to Telegram", "panic", rcv)
		}
	}()
	n(nctx, userID, sessionID, text)
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
