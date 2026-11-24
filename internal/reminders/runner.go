package reminders

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/metrics"
)

const (
	// pollEvery is how often the sender looks for reminders that are due: a reminder comes at most
	// this late.
	pollEvery = 20 * time.Second
	// batch is how many reminders one pass sends.
	batch = 20
	// keepProposals and keepOver: how long an unanswered proposal and a reminder that is over stay.
	keepProposals = 7 * 24 * time.Hour
	keepOver      = 90 * 24 * time.Hour
)

// Notifier sends a text to the person's Telegram chat of a conversation "tg-<chat id>"
// (agent.Orchestrator.NotifyOutside).
type Notifier func(ctx context.Context, userID uuid.UUID, sessionID, text string)

// Runner sends the reminders in their hour.
type Runner struct {
	d      *db.Database
	store  *Store
	notify Notifier
	loc    *time.Location
	now    func() time.Time
	wake   chan struct{}
}

// NewRunner returns a sender of the reminders kept in store.
func NewRunner(d *db.Database, store *Store, notify Notifier) *Runner {
	return &Runner{d: d, store: store, notify: notify, loc: Location(), now: time.Now, wake: make(chan struct{}, 1)}
}

// Wake makes the sender look now (a reminder was kept). It never blocks.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run sends the reminders until ctx ends. The ones whose hour passed while the backend was off
// are sent at once, marked late.
func (r *Runner) Run(ctx context.Context) {
	slog.Info("the sender of the reminders is on")
	lastCleanup := time.Time{}
	for {
		for {
			n, err := r.Pass(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Error("the reminders could not be sent; they are tried again", "error", err)
			}
			if n < batch || err != nil {
				break
			}
		}
		if now := r.now(); now.Sub(lastCleanup) > 24*time.Hour {
			lastCleanup = now
			if n, err := r.d.Queries.DeleteOldReminders(ctx, db.DeleteOldRemindersParams{
				CreatedAt: now.Add(-keepProposals), UpdatedAt: now.Add(-keepOver),
			}); err != nil {
				slog.Warn("old reminders could not be removed", "error", err)
			} else if n > 0 {
				slog.Info("old reminders removed", "count", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(pollEvery):
		}
	}
}

// sent is a reminder written into its conversation, to be sent to Telegram after the commit.
type sent struct {
	userID  uuid.UUID
	session string
	text    string
}

// Pass sends the reminders that are due and tells how many it sent. Writing a reminder into its
// conversation and moving it to its next time are one transaction, with the row locked: a reminder
// is never written twice. Telegram gets it after the commit; a crash in between loses only that.
func (r *Runner) Pass(ctx context.Context) (int, error) {
	now := r.now()
	tx, err := r.d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := r.d.Queries.WithTx(tx)
	rows, err := q.DueReminders(ctx, db.DueRemindersParams{NextAt: now, Limit: batch})
	if err != nil {
		return 0, fmt.Errorf("read the reminders that are due: %w", err)
	}
	out := make([]sent, 0, len(rows))
	for _, row := range rows {
		rem := fromRow(row)
		text := SentText(rem.Text, rem.Rule, rem.Next, rem.Early, now, r.loc)
		if _, err := q.CreateMessage(ctx, db.CreateMessageParams{
			UserID: row.UserID, SessionID: row.SessionID, Role: "assistant", Content: text,
			SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true},
			ToolLog:        sql.NullString{String: fmt.Sprintf("reminder: sent reminder %d", row.ID), Valid: true},
		}); err != nil {
			return 0, fmt.Errorf("write reminder %d into its conversation: %w", row.ID, err)
		}
		// The next time after now: the sendings missed while the computer was off are not repeated.
		status := "active"
		next, early, ok := rem.Rule.Next(maxTime(now, rem.Next), r.loc)
		if !ok {
			status, next, early = "done", rem.Next, false
		}
		if err := q.MarkReminderSent(ctx, db.MarkReminderSentParams{ID: row.ID, Status: status, NextAt: next, NextEarly: early}); err != nil {
			return 0, fmt.Errorf("move reminder %d to its next time: %w", row.ID, err)
		}
		late := "on_time"
		if now.Sub(rem.Next) > LateAfter {
			late = "late"
		}
		metrics.Reminders.WithLabelValues("sent_" + late).Inc()
		slog.Info("a reminder was sent", "reminder_id", row.ID, "late", late == "late", "next", status == "active")
		out = append(out, sent{userID: row.UserID, session: row.SessionID, text: text})
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	for _, s := range out {
		r.toTelegram(ctx, s)
	}
	return len(rows), nil
}

// toTelegram sends a reminder to the person's chat: the chat of its conversation, or, for a
// conversation of the app only, the chat linked to the person.
func (r *Runner) toTelegram(ctx context.Context, s sent) {
	if r.notify == nil {
		return
	}
	session := s.session
	if !api.IsTelegramSession(session) {
		chat, ok, err := r.store.TelegramChat(ctx, s.userID)
		if err != nil {
			slog.Warn("the Telegram chat of a reminder could not be read", "error", err)
			return
		}
		if !ok {
			return
		}
		session = api.TelegramSessionID(chat)
	}
	r.notify(ctx, s.userID, session, s.text)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
