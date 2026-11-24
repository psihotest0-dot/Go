package bills

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/reminders"
	"go-monolith/metrics"
)

// The monthly report (phase 2): after 09:00 on the 1st, once a month, the bills of the month
// before - the total, by kind, the change against the month before it, the unpaid ones - go into
// the person's conversation and to Telegram. A backend that was off on the 1st sends it when it
// starts. Off unless BILLS_REPORT_ENABLED.

const (
	reportEvery  = 10 * time.Minute
	reportMinute = 9 * 60
)

// Notifier sends a text to the Telegram chat of a conversation "tg-<chat id>".
type Notifier func(ctx context.Context, userID uuid.UUID, sessionID, text string)

// RunReports sends the monthly reports until ctx ends.
func (s *Service) RunReports(ctx context.Context, notify Notifier) {
	slog.Info("the monthly report of the bills is on")
	for {
		if err := s.ReportPass(ctx, notify); err != nil && ctx.Err() == nil {
			slog.Error("the monthly reports of the bills could not be sent", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reportEvery):
		}
	}
}

// ReportPass sends the report of last month to everyone who has not had it, once it is time.
func (s *Service) ReportPass(ctx context.Context, notify Notifier) error {
	now := s.now()
	today := reminders.DayOf(now, s.loc)
	first := reminders.Day{Year: today.Year, Month: today.Month, Day: 1}
	if now.Before(first.Time(reportMinute, s.loc)) {
		return nil
	}
	prevDay := first.AddDays(-1)
	last := monthPeriod(prevDay.Year, prevDay.Month)
	beforeDay := last.From.AddDays(-1)
	before := monthPeriod(beforeDay.Year, beforeDay.Month)

	users, err := s.q.UsersWithKeptBills(ctx)
	if err != nil {
		return err
	}
	for _, userID := range users {
		bills, err := s.between(ctx, userID, last, "")
		if err != nil {
			return err
		}
		unpaid, _, err := s.Unpaid(ctx, userID)
		if err != nil {
			return err
		}
		if len(bills) == 0 && len(unpaid) == 0 {
			continue
		}
		earlier, err := s.between(ctx, userID, before, "")
		if err != nil {
			return err
		}
		session, telegram, err := s.reportSession(ctx, userID)
		if err != nil {
			return err
		}
		n, err := s.q.ClaimBillReport(ctx, db.ClaimBillReportParams{UserID: userID, Month: first.UTC(), SessionID: session})
		if err != nil {
			return err
		}
		if n == 0 {
			continue // sent already this month
		}
		text := ReportText(last, sumOf(bills), before, sumOf(earlier), unpaid, today)
		if _, err := s.q.CreateMessage(ctx, db.CreateMessageParams{
			UserID: userID, SessionID: session, Role: "assistant", Content: text,
			SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true},
			ToolLog:        sql.NullString{String: "bills: the monthly report of " + last.Label, Valid: true},
		}); err != nil {
			_ = s.q.UnclaimBillReport(ctx, db.UnclaimBillReportParams{UserID: userID, Month: first.UTC()})
			return fmt.Errorf("write the monthly report: %w", err)
		}
		metrics.Bills.WithLabelValues("report").Inc()
		slog.Info("the monthly report of the bills was sent", "month", last.Label, "telegram", telegram)
		if telegram && notify != nil {
			notify(ctx, userID, session, text)
		}
	}
	return nil
}

// reportSession is the conversation of a report: the person's Telegram conversation when there is
// one (it is shared with the app), else the conversation of their newest bill.
func (s *Service) reportSession(ctx context.Context, userID uuid.UUID) (string, bool, error) {
	chat, err := s.q.TelegramChatOfUser(ctx, userID)
	if err == nil {
		return api.TelegramSessionID(chat), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	session, err := s.q.LatestBillSession(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "bills", false, nil
	}
	return session, false, err
}

// ReportText is the report of a month.
func ReportText(month Period, s Sum, before Period, b Sum, unpaid []Bill, today reminders.Day) string {
	var t strings.Builder
	fmt.Fprintf(&t, "📊 Сметките за %s: общо %s (%s).\n", month.Label, Money(s.Cents, "EUR"), countWord(s.Count))
	if len(s.ByKind) > 1 {
		for _, k := range s.kindsByAmount() {
			fmt.Fprintf(&t, "• %s — %s\n", KindNames[k], Money(s.ByKind[k], "EUR"))
		}
	}
	if s.Count > 0 && b.Count > 0 {
		fmt.Fprintf(&t, "Спрямо %s (%s): %s.\n", before.Label, Money(b.Cents, "EUR"), change(s.Cents, b.Cents))
	}
	if note := biggestChangeNote(s, b); note != "" {
		t.WriteString(note + "\n")
	} else if note := biggestKindNote(s); note != "" {
		t.WriteString(note + "\n")
	}
	if len(unpaid) > 0 {
		var cents int64
		for _, u := range unpaid {
			cents += u.Cents
		}
		fmt.Fprintf(&t, "Неплатени сега: %s, %s:\n", countWord(len(unpaid)), Money(cents, "EUR"))
		for i, u := range unpaid {
			if i == listedBills {
				fmt.Fprintf(&t, "… и още %d\n", len(unpaid)-listedBills)
				break
			}
			line := "• " + oneLine(u, nil)
			if u.DueDate != nil && dayOfTime(*u.DueDate).Before(today) {
				line += " ⚠️ срокът мина"
			}
			t.WriteString(line + "\n")
		}
	}
	t.WriteString("Питай ме: „колко платих за ток тази година?“, „сравни ноември с октомври“, „кои не съм платил?“.")
	return t.String()
}
