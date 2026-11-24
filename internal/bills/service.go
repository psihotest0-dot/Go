package bills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/db"
	"go-monolith/internal/reminders"
	"go-monolith/metrics"
)

// MaxFileBytes bounds an uploaded bill (a scan of a few pages is 1-3 MB).
const MaxFileBytes = 20 << 20

// minTextRunes: less text than this is an empty scan, not a bill.
const minTextRunes = 30

// textReader turns a file into its text (ingest.PDFExtractor, ingest.ImageOCRExtractor).
type textReader interface {
	Extract(ctx context.Context, r io.Reader) (string, error)
}

// Service reads bills, keeps them, and makes the reminders of their due days.
type Service struct {
	d         *db.Database
	q         *db.Queries
	pdf       textReader
	image     textReader
	extractor *Extractor
	reminders *reminders.Store
	wake      func()
	loc       *time.Location
	now       func() time.Time
}

// NewService returns the bills kept in d: pdf and image read the files, extractor the fields,
// rem keeps the reminders (wake tells their sender that one was kept).
func NewService(d *db.Database, pdf, image textReader, extractor *Extractor, rem *reminders.Store, wake func()) *Service {
	return &Service{d: d, q: d.Queries, pdf: pdf, image: image, extractor: extractor, reminders: rem, wake: wake,
		loc: reminders.Location(), now: time.Now}
}

// Read reads an uploaded bill and answers with what it found and the question whether to keep it;
// the upload and the answer are written into the conversation, so that the person's "да" in the
// chat keeps it (Answer).
func (s *Service) Read(ctx context.Context, userID uuid.UUID, session, name string, data []byte) (string, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." {
		name = "сметка"
	}
	text := s.read(ctx, userID, session, name, data)
	if err := s.say(ctx, userID, session, "📎 Качих сметка: "+name, text); err != nil {
		return text, err
	}
	return text, nil
}

func (s *Service) read(ctx context.Context, userID uuid.UUID, session, name string, data []byte) string {
	if len(data) == 0 {
		return "Файлът е празен."
	}
	if len(data) > MaxFileBytes {
		return fmt.Sprintf("Файлът е над %d MB — не мога да го прочета като сметка.", MaxFileBytes>>20)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if kept, err := s.q.KeptBillWithFile(ctx, db.KeptBillWithFileParams{UserID: userID, FileHash: hash}); err == nil {
		metrics.Bills.WithLabelValues("same_file").Inc()
		return "Тази сметка вече е записана (същият файл): " + oneLine(fromRow(kept), s.loc) + "."
	}

	reader := s.readerFor(name, data)
	if reader == nil {
		metrics.Bills.WithLabelValues("unsupported").Inc()
		return "Мога да чета сметки като PDF или снимка (JPG, PNG). Този файл не е от тях."
	}
	text, err := reader.Extract(ctx, bytes.NewReader(data))
	if err != nil {
		metrics.Bills.WithLabelValues("unread").Inc()
		slog.WarnContext(ctx, "a bill could not be read", "err", err)
		return "Не успях да прочета файла (PDF или снимка). Опитай с друг файл или с по-ясен скан."
	}
	if utf8.RuneCountInString(strings.TrimSpace(text)) < minTextRunes {
		metrics.Bills.WithLabelValues("no_text").Inc()
		return "Не намерих текст в документа — сканът е празен или много неясен. Опитай с по-ясен скан (300 dpi)."
	}
	fields, err := s.extractor.Extract(ctx, text)
	if err != nil {
		metrics.Bills.WithLabelValues("extract_failed").Inc()
		slog.WarnContext(ctx, "the fields of a bill could not be read", "err", err)
		return "Прочетох документа, но моделът не успя да извади полетата на сметката. Опитай пак след малко. Нищо не е записано."
	}
	b, problem := Verify(fields, text)
	switch problem {
	case NotABill:
		metrics.Bills.WithLabelValues("not_a_bill").Inc()
		return "Това не прилича на сметка или фактура, затова не я записвам."
	case AmountMissing:
		metrics.Bills.WithLabelValues("amount_missing").Inc()
		return "Не намерих в текста сумата за плащане, затова не записвам сметката (записвам само суми, които са в документа). " +
			"Ако е скан, опитай с по-ясен."
	}
	for _, d := range b.Dropped {
		metrics.Bills.WithLabelValues("dropped_" + d).Inc()
	}

	similar, err := s.q.SimilarKeptBills(ctx, db.SimilarKeptBillsParams{
		UserID: userID, AmountCents: b.Cents, InvoiceDate: nullDate(b.InvoiceDate), DueDate: nullDate(b.DueDate), PaidDate: nullDate(b.PaidDate),
	})
	if err != nil {
		slog.WarnContext(ctx, "could not look for the same bill", "err", err)
	}

	tx, err := s.d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко."
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if _, err := q.DropProposedBill(ctx, db.DropProposedBillParams{UserID: userID, SessionID: session}); err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко."
	}
	if _, err := q.ProposeBill(ctx, db.ProposeBillParams{
		UserID: userID, SessionID: session, Issuer: b.Issuer, Kind: b.Kind, AmountCents: b.Cents, Currency: b.Currency,
		InvoiceDate: nullDate(b.InvoiceDate), DueDate: nullDate(b.DueDate), PaidDate: nullDate(b.PaidDate),
		Payment: b.Payment, Repeat: b.Repeat, Summary: b.Summary, FileName: name, FileHash: hash,
	}); err != nil {
		slog.WarnContext(ctx, "a bill could not be proposed", "err", err)
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко."
	}
	if err := tx.Commit(); err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко."
	}
	metrics.Bills.WithLabelValues("proposed").Inc()
	// The "да" that follows is for the bill: a reminder that waited in the conversation is dropped.
	if s.reminders != nil {
		_ = s.reminders.DropProposed(ctx, userID, session)
	}
	var same []Bill
	for _, r := range similar {
		same = append(same, fromRow(r))
	}
	return ProposalText(name, b, same, s.now(), s.loc)
}

// readerFor picks the reader of a file by its name and its first bytes.
func (s *Service) readerFor(name string, data []byte) textReader {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case bytes.HasPrefix(data, []byte("%PDF")) || ext == ".pdf":
		return s.pdf
	case ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".tif" || ext == ".tiff" || ext == ".webp" ||
		bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) || bytes.HasPrefix(data, []byte("\x89PNG")):
		return s.image
	}
	return nil
}

// say writes the upload and the answer into the conversation.
func (s *Service) say(ctx context.Context, userID uuid.UUID, session, upload, answer string) error {
	for _, m := range [][2]string{{"user", upload}, {"assistant", answer}} {
		params := db.CreateMessageParams{UserID: userID, SessionID: session, Role: m[0], Content: m[1],
			SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true}}
		if m[0] == "assistant" {
			params.ToolLog = sql.NullString{String: "bills: read an uploaded bill", Valid: true}
		}
		if _, err := s.q.CreateMessage(ctx, params); err != nil {
			return fmt.Errorf("write the bill into the conversation: %w", err)
		}
	}
	return nil
}

// Proposed is the bill of the conversation that waits for the person's yes, or nil.
func (s *Service) Proposed(ctx context.Context, userID uuid.UUID, session string) (*Bill, error) {
	row, err := s.q.GetProposedBill(ctx, db.GetProposedBillParams{UserID: userID, SessionID: session})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b := fromRow(row)
	return &b, nil
}

// Drop forgets the bill that waits in the conversation.
func (s *Service) Drop(ctx context.Context, userID uuid.UUID, session string) error {
	_, err := s.q.DropProposedBill(ctx, db.DropProposedBillParams{UserID: userID, SessionID: session})
	return err
}

// Keep keeps the bill that waits in the conversation, with the reminder of its due day when it
// needs one, and says what was done.
func (s *Service) Keep(ctx context.Context, userID uuid.UUID, session string, p Bill) string {
	now := s.now()
	var reminderID sql.NullInt64
	plan := PlanReminder(p, now, s.loc)
	var kept reminders.Reminder
	if plan.Text != "" && s.reminders != nil {
		r, err := s.reminders.CreateAt(ctx, userID, session, plan.Text, plan.At, s.loc)
		if err != nil {
			slog.WarnContext(ctx, "the reminder of a bill could not be kept", "err", err)
			plan = ReminderPlan{Why: "напомнянето не можа да се запише (грешка в базата)"}
		} else {
			kept, reminderID = r, sql.NullInt64{Int64: r.ID, Valid: true}
		}
	}
	if _, err := s.q.KeepBill(ctx, db.KeepBillParams{ID: p.ID, UserID: userID, ReminderID: reminderID}); err != nil {
		if reminderID.Valid {
			_, _ = s.reminders.Cancel(ctx, userID, kept.ID)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return "Тази сметка вече е записана (или отказана)."
		}
		if strings.Contains(err.Error(), "idx_bills_file") {
			return "Тази сметка вече е записана (същият файл)."
		}
		slog.WarnContext(ctx, "a bill could not be kept", "err", err)
		return "Не успях да запиша сметката (грешка в базата). Опитай пак след малко."
	}
	metrics.Bills.WithLabelValues("kept").Inc()
	text := "Записах сметката ✅ " + oneLine(p, s.loc) + "."
	if reminderID.Valid {
		if s.wake != nil {
			s.wake()
		}
		metrics.Bills.WithLabelValues("reminder").Inc()
		text += fmt.Sprintf("\n⏰ Ще ти напомня %s.", reminders.OnText(plan.At, now, s.loc))
	} else if plan.Why != "" {
		text += "\nБез напомняне: " + plan.Why + "."
	}
	return text
}

func nullDate(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

func datePtr(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Date(n.Time.Year(), n.Time.Month(), n.Time.Day(), 0, 0, 0, 0, time.UTC)
	return &t
}

func fromRow(r db.Bill) Bill {
	return Bill{ID: r.ID, Issuer: r.Issuer, Kind: r.Kind, Cents: r.AmountCents, Currency: r.Currency,
		InvoiceDate: datePtr(r.InvoiceDate), DueDate: datePtr(r.DueDate), PaidDate: datePtr(r.PaidDate),
		Payment: r.Payment, Repeat: r.Repeat, Summary: r.Summary, FileName: r.FileName}
}

// unpaidLimit bounds the bills "платих …" is matched against.
const unpaidLimit = 200

// Unpaid are the kept bills the person still has to pay (by transfer, or of an unknown way), the
// nearest due day first.
func (s *Service) Unpaid(ctx context.Context, userID uuid.UUID) ([]Bill, []db.Bill, error) {
	rows, err := s.q.ListKeptBills(ctx, db.ListKeptBillsParams{UserID: userID, Limit: unpaidLimit})
	if err != nil {
		return nil, nil, err
	}
	var out []Bill
	var raw []db.Bill
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.PaidDate.Valid || r.Payment == Paid || r.Payment == DirectDebit {
			continue
		}
		out = append(out, fromRow(r))
		raw = append(raw, r)
	}
	return out, raw, nil
}

// MarkPaid answers "платих сметката за зъболекаря" / "платих 2": the bill is marked paid today and
// the reminder of its due day is cancelled. handled is false when the person has no unpaid bill,
// so the model answers the message.
func (s *Service) MarkPaid(ctx context.Context, userID uuid.UUID, message string) (text string, handled bool) {
	unpaid, raw, err := s.Unpaid(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the unpaid bills", "err", err)
		return "", false
	}
	if len(unpaid) == 0 {
		return "", false
	}
	pick := pickBills(unpaid, message)
	switch len(pick) {
	case 1:
		i := pick[0]
		today := reminders.DayOf(s.now(), s.loc).UTC()
		n, err := s.q.MarkBillPaid(ctx, db.MarkBillPaidParams{ID: unpaid[i].ID, UserID: userID, PaidDate: sql.NullTime{Time: today, Valid: true}})
		if err != nil || n == 0 {
			return "Не успях да отбележа сметката като платена. Опитай пак след малко.", true
		}
		if raw[i].ReminderID.Valid && s.reminders != nil {
			_, _ = s.reminders.Cancel(ctx, userID, raw[i].ReminderID.Int64)
		}
		metrics.Bills.WithLabelValues("paid").Inc()
		return "Отбелязах като платена ✅ " + oneLine(unpaid[i], s.loc) + ". Напомнянето ѝ е махнато.", true
	}
	var b strings.Builder
	if len(pick) == 0 {
		b.WriteString("Коя сметка плати? Неплатените:\n")
		pick = make([]int, len(unpaid))
		for i := range unpaid {
			pick[i] = i
		}
	} else {
		b.WriteString("Коя от тези плати?\n")
	}
	for _, i := range pick {
		fmt.Fprintf(&b, "%d. %s\n", i+1, oneLine(unpaid[i], s.loc))
	}
	b.WriteString("Кажи „платих“ и номера, например „платих 1“.")
	return b.String(), true
}

// pickBills are the indexes of the bills the message names: by its number in the list, or by the
// words it shares with the issuer, the summary and the kind.
func pickBills(unpaid []Bill, message string) []int {
	if m := listNumberRe.FindStringSubmatch(message); m != nil {
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		if n >= 1 && n <= len(unpaid) {
			return []int{n - 1}
		}
	}
	want := stems(message)
	best, bestScore := []int(nil), 0
	for i, b := range unpaid {
		score := 0
		for st := range stems(b.Issuer + " " + b.Summary + " " + KindNames[b.Kind]) {
			if want[st] {
				score++
			}
		}
		switch {
		case score == 0:
		case score > bestScore:
			best, bestScore = []int{i}, score
		case score == bestScore:
			best = append(best, i)
		}
	}
	return best
}

// SetClock sets the clock of the service (for the tests of other packages).
func SetClock(s *Service, now func() time.Time) { s.now = now }
