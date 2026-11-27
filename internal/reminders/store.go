package reminders

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"go-monolith/internal/db"
)

// Store keeps the reminders in PostgreSQL.
type Store struct {
	d *db.Database
	q *db.Queries
}

// NewStore returns the reminders stored in d.
func NewStore(d *db.Database) *Store { return &Store{d: d, q: d.Queries} }

// Reminder is a reminder as the code works with it.
type Reminder struct {
	ID      int64
	Session string
	Text    string
	Rule    Rule
	Next    time.Time
	Early   bool
}

func fromRow(r db.Reminder) Reminder {
	return Reminder{
		ID: r.ID, Session: r.SessionID, Text: r.Text, Next: r.NextAt, Early: r.NextEarly,
		Rule: Rule{First: DayFromUTC(r.FirstDate), Minute: int(r.AtMinute), Repeat: r.Repeat, DayBefore: r.DayBefore},
	}
}

// Propose keeps the draft as the proposal of the conversation, in the place of an earlier one.
func (s *Store) Propose(ctx context.Context, userID uuid.UUID, session string, d Draft) (Reminder, error) {
	tx, err := s.d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return Reminder{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if _, err := q.DropProposedReminder(ctx, db.DropProposedReminderParams{UserID: userID, SessionID: session}); err != nil {
		return Reminder{}, fmt.Errorf("drop the earlier proposal: %w", err)
	}
	row, err := q.ProposeReminder(ctx, db.ProposeReminderParams{
		UserID: userID, SessionID: session, Text: d.Text, Repeat: d.Rule.Repeat, FirstDate: d.Rule.First.UTC(),
		AtMinute: int32(d.Rule.Minute), DayBefore: d.Rule.DayBefore, NextAt: d.Next, NextEarly: d.Early,
	})
	if err != nil {
		return Reminder{}, fmt.Errorf("store the proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Reminder{}, fmt.Errorf("commit: %w", err)
	}
	return fromRow(row), nil
}

// Proposed is the proposal of the conversation, or nil.
func (s *Store) Proposed(ctx context.Context, userID uuid.UUID, session string) (*Reminder, error) {
	row, err := s.q.GetProposedReminder(ctx, db.GetProposedReminderParams{UserID: userID, SessionID: session})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the proposal: %w", err)
	}
	r := fromRow(row)
	return &r, nil
}

// DropProposed forgets the proposal of the conversation.
func (s *Store) DropProposed(ctx context.Context, userID uuid.UUID, session string) error {
	_, err := s.q.DropProposedReminder(ctx, db.DropProposedReminderParams{UserID: userID, SessionID: session})
	return err
}

// ErrGone: the proposal is not there any more (a second yes, or it was dropped).
var ErrGone = errors.New("the proposal is not there any more")

// Activate keeps the proposal, with its next sending computed again at now (the yes may come a
// while after the question). ok is false when the time has passed meanwhile.
func (s *Store) Activate(ctx context.Context, userID uuid.UUID, p Reminder, now time.Time, loc *time.Location) (Reminder, bool, error) {
	next, early, ok := p.Rule.Next(now, loc)
	if !ok {
		if err := s.DropProposed(ctx, userID, p.Session); err != nil {
			slog.WarnContext(ctx, "a reminder whose time has passed could not be dropped", "err", err)
		}
		return p, false, nil
	}
	row, err := s.q.ActivateReminder(ctx, db.ActivateReminderParams{ID: p.ID, UserID: userID, NextAt: next, NextEarly: early})
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, ErrGone
	}
	if err != nil {
		return p, false, fmt.Errorf("keep the reminder: %w", err)
	}
	return fromRow(row), true, nil
}

// Active are the person's reminders that are kept, the next first.
func (s *Store) Active(ctx context.Context, userID uuid.UUID) ([]Reminder, error) {
	rows, err := s.q.ListActiveReminders(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list the reminders: %w", err)
	}
	out := make([]Reminder, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// Cancel removes a kept reminder; ok is false when it was not there.
func (s *Store) Cancel(ctx context.Context, userID uuid.UUID, id int64) (bool, error) {
	n, err := s.q.CancelReminder(ctx, db.CancelReminderParams{ID: id, UserID: userID})
	return n > 0, err
}

// TelegramChat is the person's Telegram chat, ok false when none is linked.
func (s *Store) TelegramChat(ctx context.Context, userID uuid.UUID) (int64, bool, error) {
	id, err := s.q.TelegramChatOfUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// CreateAt keeps a reminder for one time at once, without a proposal: the person said yes to it
// already (the due day of a bill they kept).
func (s *Store) CreateAt(ctx context.Context, userID uuid.UUID, session, text string, at time.Time, loc *time.Location) (Reminder, error) {
	local := at.In(loc)
	row, err := s.q.CreateActiveReminder(ctx, db.CreateActiveReminderParams{
		UserID: userID, SessionID: session, Text: text, FirstDate: DayOf(at, loc).UTC(),
		AtMinute: int32(local.Hour()*60 + local.Minute()), NextAt: at,
	})
	if err != nil {
		return Reminder{}, fmt.Errorf("keep the reminder: %w", err)
	}
	return fromRow(row), nil
}
