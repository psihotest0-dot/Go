package bills

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"go-monolith/internal/db"
	"go-monolith/internal/reminders"
	"go-monolith/metrics"
)

// listedBills is the most bills a list names one by one.
const listedBills = 20

// Sum is the bills of a period: their total, how many, and the same by kind.
type Sum struct {
	Cents  int64
	Count  int
	ByKind map[string]int64
	Kinds  map[string]int
	Unpaid []Bill
	Bills  []Bill
}

func sumOf(bills []Bill) Sum {
	s := Sum{ByKind: map[string]int64{}, Kinds: map[string]int{}}
	for _, b := range bills {
		s.Cents += b.Cents
		s.Count++
		s.ByKind[b.Kind] += b.Cents
		s.Kinds[b.Kind]++
		if b.PaidDate == nil && b.Payment != Paid && b.Payment != DirectDebit {
			s.Unpaid = append(s.Unpaid, b)
		}
	}
	s.Bills = bills
	return s
}

// kindsByAmount are the kinds of a sum, the largest first.
func (s Sum) kindsByAmount() []string {
	kinds := make([]string, 0, len(s.ByKind))
	for k := range s.ByKind {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if s.ByKind[kinds[i]] != s.ByKind[kinds[j]] {
			return s.ByKind[kinds[i]] > s.ByKind[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	return kinds
}

// between are the kept bills of a period, of one kind when kind is not "".
func (s *Service) between(ctx context.Context, userID uuid.UUID, p Period, kind string) ([]Bill, error) {
	rows, err := s.q.KeptBillsBetween(ctx, db.KeptBillsBetweenParams{UserID: userID, FromDay: p.From.UTC(), ToDay: p.To.UTC()})
	if err != nil {
		return nil, err
	}
	var out []Bill
	for _, r := range rows {
		if kind == "" || r.Kind == kind {
			out = append(out, fromRow(r))
		}
	}
	return out, nil
}

// Answer is the answer of the code to a question about the bills, handled false when the message
// asks no such question (the model answers it).
func (s *Service) Answer(ctx context.Context, userID uuid.UUID, message string) (text string, handled bool) {
	today := reminders.DayOf(s.now(), s.loc)
	q, ok := ReadQuestion(message, today)
	if !ok {
		return "", false
	}
	n, err := s.q.CountKeptBills(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not count the bills", "err", err)
		return "", false
	}
	if n == 0 {
		metrics.Bills.WithLabelValues("question_no_bills").Inc()
		return "Още нямаш записани сметки, затова нямам какво да събера. Качи сметка с бутона „Сметка“ във Flet " +
			"или прати PDF / снимка на бота в Telegram — после ще мога да ти казвам сборове, неплатени и сравнения.", true
	}
	text, err = s.answer(ctx, userID, q, today)
	if err != nil {
		slog.WarnContext(ctx, "a question about the bills could not be answered", "err", err)
		return "Не успях да прочета сметките (грешка в базата). Опитай пак след малко.", true
	}
	metrics.Bills.WithLabelValues("question_" + q.Ask).Inc()
	return text, true
}

func kindLabel(kind string) string {
	if kind == "" {
		return ""
	}
	return " за " + KindNames[kind]
}

func (s *Service) answer(ctx context.Context, userID uuid.UUID, q Question, today reminders.Day) (string, error) {
	switch q.Ask {
	case AskUnpaid:
		unpaid, _, err := s.Unpaid(ctx, userID)
		if err != nil {
			return "", err
		}
		return unpaidText(unpaid, q, today), nil
	case AskCompare:
		a, err := s.between(ctx, userID, q.Period, q.Kind)
		if err != nil {
			return "", err
		}
		b, err := s.between(ctx, userID, *q.Other, q.Kind)
		if err != nil {
			return "", err
		}
		return compareText(q, sumOf(a), sumOf(b)), nil
	}
	bills, err := s.between(ctx, userID, q.Period, q.Kind)
	if err != nil {
		return "", err
	}
	sum := sumOf(bills)
	if sum.Count == 0 {
		return fmt.Sprintf("Нямам записани сметки%s за %s.", kindLabel(q.Kind), q.Period.Label), nil
	}
	if q.Ask == AskList {
		return listText(q, sum), nil
	}
	return totalText(q, sum), nil
}

func countWord(n int) string {
	if n == 1 {
		return "1 сметка"
	}
	return fmt.Sprintf("%d сметки", n)
}

func totalText(q Question, s Sum) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Сметките%s за %s: общо %s (%s).\n", kindLabel(q.Kind), q.Period.Label, Money(s.Cents, "EUR"), countWord(s.Count))
	if q.Kind == "" && len(s.ByKind) > 1 {
		for _, k := range s.kindsByAmount() {
			fmt.Fprintf(&b, "• %s — %s (%s)\n", KindNames[k], Money(s.ByKind[k], "EUR"), countWord(s.Kinds[k]))
		}
	}
	writeUnpaid(&b, s.Unpaid)
	if q.Kind == "" {
		writeNote(&b, biggestKindNote(s))
	}
	return strings.TrimRight(b.String(), "\n")
}

func listText(q Question, s Sum) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Сметките%s за %s (%s, общо %s):\n", kindLabel(q.Kind), q.Period.Label, countWord(s.Count), Money(s.Cents, "EUR"))
	for i, bill := range s.Bills {
		if i == listedBills {
			fmt.Fprintf(&b, "… и още %d\n", len(s.Bills)-listedBills)
			break
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, oneLine(bill, nil))
	}
	writeUnpaid(&b, s.Unpaid)
	return strings.TrimRight(b.String(), "\n")
}

func writeUnpaid(b *strings.Builder, unpaid []Bill) {
	if len(unpaid) == 0 {
		return
	}
	var cents int64
	for _, u := range unpaid {
		cents += u.Cents
	}
	fmt.Fprintf(b, "Неплатени от тях: %s (%s) — „кои не съм платил?“ ги изрежда.\n", countWord(len(unpaid)), Money(cents, "EUR"))
}

func unpaidText(unpaid []Bill, q Question, today reminders.Day) string {
	var keep []Bill
	for _, u := range unpaid {
		if q.Kind != "" && u.Kind != q.Kind {
			continue
		}
		if q.Period.Label != "" {
			d := u.DueDate
			if d == nil {
				d = u.InvoiceDate
			}
			if d == nil || dayOfTime(*d).Before(q.Period.From) || q.Period.To.Before(dayOfTime(*d)) {
				continue
			}
		}
		keep = append(keep, u)
	}
	if len(keep) == 0 {
		return fmt.Sprintf("Нямаш неплатени сметки%s — всички записани са платени или се изтеглят автоматично (директен дебит).", kindLabel(q.Kind))
	}
	var b strings.Builder
	var cents int64
	for _, u := range keep {
		cents += u.Cents
	}
	fmt.Fprintf(&b, "Неплатени%s: %s, общо %s:\n", kindLabel(q.Kind), countWord(len(keep)), Money(cents, "EUR"))
	for i, u := range keep {
		line := fmt.Sprintf("%d. %s", i+1, oneLine(u, nil))
		if u.DueDate != nil && dayOfTime(*u.DueDate).Before(today) {
			line += " ⚠️ срокът мина"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("Когато платиш някоя: „платих 1“ или „платих тока“.")
	return b.String()
}

func dayOfTime(t time.Time) reminders.Day {
	return reminders.Day{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// change is "+12,40 € (+18 %)" or "−3,10 € (−5 %)".
func change(now, before int64) string {
	diff := now - before
	sign := "+"
	if diff < 0 {
		sign, diff = "−", -diff
	}
	text := sign + Money(diff, "EUR")
	if before > 0 {
		pct := (now - before) * 100 / before
		if pct < 0 {
			pct = -pct
		}
		text += fmt.Sprintf(" (%s%d %%)", sign, pct)
	}
	return text
}

func compareText(q Question, a, b Sum) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Сметките%s: %s — %s (%s); %s — %s (%s).\n", kindLabel(q.Kind),
		q.Period.Label, Money(a.Cents, "EUR"), countWord(a.Count), q.Other.Label, Money(b.Cents, "EUR"), countWord(b.Count))
	if a.Count > 0 && b.Count > 0 {
		fmt.Fprintf(&s, "Разлика: %s спрямо %s.\n", change(a.Cents, b.Cents), q.Other.Label)
	}
	if q.Kind == "" {
		kinds := map[string]bool{}
		for k := range a.ByKind {
			kinds[k] = true
		}
		for k := range b.ByKind {
			kinds[k] = true
		}
		names := make([]string, 0, len(kinds))
		for k := range kinds {
			names = append(names, k)
		}
		sort.Strings(names)
		if len(names) > 1 {
			for _, k := range names {
				line := fmt.Sprintf("• %s: %s — %s", KindNames[k], Money(a.ByKind[k], "EUR"), Money(b.ByKind[k], "EUR"))
				if a.ByKind[k] > 0 && b.ByKind[k] > 0 && a.ByKind[k] != b.ByKind[k] {
					line += " (" + change(a.ByKind[k], b.ByKind[k]) + ")"
				}
				s.WriteString(line + "\n")
			}
		}
	}
	if q.Kind == "" {
		writeNote(&s, biggestChangeNote(a, b))
	}
	if a.Count == 0 || b.Count == 0 {
		s.WriteString("(За единия период няма записани сметки, затова не правя сравнение.)\n")
	}
	return strings.TrimRight(s.String(), "\n")
}
