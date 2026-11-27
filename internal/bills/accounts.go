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
	// Cents and Count are of the bills in euro (and in lev, at its fixed rate); Foreign counts the
	// bills of another currency, which are not in them (they are in Bills and Unpaid).
	Cents   int64
	Count   int
	Foreign int
	ByKind  map[string]int64
	Kinds   map[string]int
	Unpaid  []Bill
	Bills   []Bill
}

func sumOf(bills []Bill) Sum {
	s := Sum{ByKind: map[string]int64{}, Kinds: map[string]int{}}
	for _, b := range bills {
		if cents, ok := euroCents(b.Cents, b.Currency); ok {
			s.Cents += cents
			s.Count++
			s.ByKind[b.Kind] += cents
			s.Kinds[b.Kind]++
		} else {
			s.Foreign++
		}
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

// allKept are the kept bills of every time, of one kind when kind is not "", the earliest first.
// All of them: a sum of all time over the newest 200 was wrong once there were more.
func (s *Service) allKept(ctx context.Context, userID uuid.UUID, kind string) ([]Bill, error) {
	rows, err := s.q.AllKeptBills(ctx, userID)
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
	standing := s.StandingLines(ctx, userID)
	if q.Ask == AskWhere {
		metrics.Bills.WithLabelValues("question_" + q.Ask).Inc()
		return s.whereText(ctx, userID, n, standing), true
	}
	if q.Ask == AskRepeat {
		text, err := s.repeatText(ctx, userID)
		if err != nil {
			slog.WarnContext(ctx, "the payments by how often they come could not be summed", "err", err)
			return "Не успях да прочета сметките (грешка в базата). Опитай пак след малко.", true
		}
		metrics.Bills.WithLabelValues("question_" + q.Ask).Inc()
		return text, true
	}
	if n == 0 && standing != "" {
		return "Още нямаш записани плащания — първото от постоянните сметки се записва няколко дни преди деня му.\n" + standing, true
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
	if q.Ask == AskList && standing != "" {
		text += "\n\n" + standing
	}
	metrics.Bills.WithLabelValues("question_" + q.Ask).Inc()
	return text, true
}

// repeatLine is one payment of the answer by how often they come.
type repeatLine struct {
	text  string
	cents int64
}

// newRepeatLine is a line of the payments in euro; a payment of another currency is named and left
// out of the sums.
func newRepeatLine(text string, cents int64, currency string) repeatLine {
	c, ok := euroCents(cents, currency)
	if !ok {
		return repeatLine{text: text + " — друга валута, не е в сбора"}
	}
	return repeatLine{text: text, cents: c}
}

// repeatText is the monthly and the yearly payments and what they make in a year: the standing
// bills, and of the other kept bills that come monthly, quarterly or yearly the latest of each
// issuer (a bill of an issuer with a standing bill of the same kind is that standing bill's).
// repeatLatest is how many of the newest kept bills repeatText looks at for the latest of each
// issuer: more than a year and a half of bills, so a yearly one is among them.
const repeatLatest = 200

func (s *Service) repeatText(ctx context.Context, userID uuid.UUID) (string, error) {
	standing, err := s.KeptStanding(ctx, userID)
	if err != nil {
		return "", err
	}
	rows, err := s.q.ListKeptBills(ctx, db.ListKeptBillsParams{UserID: userID, Limit: repeatLatest})
	if err != nil {
		return "", err
	}
	var monthly, quarterly, yearly []repeatLine
	today := reminders.DayOf(s.now(), s.loc)
	for _, st := range standing {
		if st.Until != nil && st.Until.Before(today) {
			continue // over
		}
		line := newRepeatLine(fmt.Sprintf("%s — %s (постоянна, %s)", title(Bill{Issuer: st.Issuer, Kind: st.Kind}), Money(st.Cents, st.Currency), standingWhen(st)), st.Cents, st.Currency)
		if st.Month == 0 {
			monthly = append(monthly, line)
		} else {
			yearly = append(yearly, line)
		}
	}
	seen := map[string]bool{}
	once := 0
	for _, r := range rows { // the latest first
		// The payments of standing bills, kept or removed: a removed one is no longer paid.
		if r.FileName == standingFileName {
			continue
		}
		b := fromRow(r)
		switch b.Repeat {
		case "monthly", "quarterly", "yearly":
		default:
			once++
			continue
		}
		covered := false
		for _, st := range standing {
			if st.Kind == b.Kind && sameIssuer(st.Issuer, b.Issuer) {
				covered = true
			}
		}
		key := b.Repeat + "|" + strings.ToLower(b.Issuer) + "|" + b.Kind
		if covered || seen[key] {
			continue
		}
		seen[key] = true
		line := newRepeatLine(fmt.Sprintf("%s — %s (последната сметка: %s)", title(b), Money(b.Cents, b.Currency), oneLineDate(b)), b.Cents, b.Currency)
		switch b.Repeat {
		case "monthly":
			monthly = append(monthly, line)
		case "quarterly":
			quarterly = append(quarterly, line)
		default:
			yearly = append(yearly, line)
		}
	}
	if len(monthly)+len(quarterly)+len(yearly) == 0 {
		return "Нямам записани месечни или годишни плащания — само еднократни сметки.", nil
	}
	var b strings.Builder
	total := func(lines []repeatLine) int64 {
		var c int64
		for _, l := range lines {
			c += l.cents
		}
		return c
	}
	section := func(head string, lines []repeatLine) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s: %s\n", head, Money(total(lines), "EUR"))
		for _, l := range lines {
			b.WriteString("• " + l.text + "\n")
		}
	}
	section("Месечни плащания (на месец)", monthly)
	section("На всеки три месеца (на тримесечие)", quarterly)
	section("Годишни плащания (на година)", yearly)
	year := total(monthly)*12 + total(quarterly)*4 + total(yearly)
	fmt.Fprintf(&b, "\nЗа една година: %s", Money(year, "EUR"))
	var parts []string
	if len(monthly) > 0 {
		parts = append(parts, Money(total(monthly), "EUR")+" × 12")
	}
	if len(quarterly) > 0 {
		parts = append(parts, Money(total(quarterly), "EUR")+" × 4")
	}
	if len(yearly) > 0 {
		parts = append(parts, Money(total(yearly), "EUR"))
	}
	if len(parts) > 1 || len(monthly)+len(quarterly) > 0 {
		b.WriteString(" (" + strings.Join(parts, " + ") + ")")
	}
	b.WriteString(".")
	if once > 0 {
		fmt.Fprintf(&b, "\nЕднократните сметки (%d) не са в тази сметка.", once)
	}
	return b.String(), nil
}

// oneLineDate is the day of a bill: its due day, else its own day.
func oneLineDate(b Bill) string {
	switch {
	case b.DueDate != nil:
		return "срок " + dayText(*b.DueDate)
	case b.InvoiceDate != nil:
		return "от " + dayText(*b.InvoiceDate)
	case b.PaidDate != nil:
		return "платена на " + dayText(*b.PaidDate)
	}
	return "без дата"
}

// whereLatest is how many of the latest bills the answer about where they are kept names.
const whereLatest = 10

// whereText says where the bills are kept: in the database, by the code, with the latest of them.
func (s *Service) whereText(ctx context.Context, userID uuid.UUID, count int64, standing string) string {
	var b strings.Builder
	b.WriteString("Сметките, които приемеш с „да“, ги пази кодът в базата данни (не във файл): оттам са сборовете, " +
		"справките, „кои не съм платил?“ и напомнянията.\n")
	if count == 0 {
		b.WriteString("Сега там няма записани сметки.\n")
	} else {
		fmt.Fprintf(&b, "Сега там има %s", countWord(int(count)))
		rows, err := s.q.ListKeptBills(ctx, db.ListKeptBillsParams{UserID: userID, Limit: whereLatest})
		if err != nil {
			slog.WarnContext(ctx, "the latest bills could not be read for the answer where they are", "err", err)
		}
		if err == nil && len(rows) > 0 {
			if count > whereLatest {
				fmt.Fprintf(&b, ", последните %d", whereLatest)
			}
			b.WriteString(":\n")
			for i, r := range rows {
				fmt.Fprintf(&b, "%d. %s\n", i+1, oneLine(fromRow(r), s.loc))
			}
		} else {
			b.WriteString(".\n")
		}
	}
	if standing != "" {
		b.WriteString(standing + "\n")
	}
	b.WriteString("Самият PDF или снимка не се пазят — пазят се прочетените данни. Текстовите файлове в „Бележките на Лили“ " +
		"(например bills.txt) са само бележки: кодът не ги чете като сметки и те не влизат в сборовете.")
	return b.String()
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
	var bills []Bill
	var err error
	if q.AllTime {
		bills, err = s.allKept(ctx, userID, q.Kind)
	} else {
		bills, err = s.between(ctx, userID, q.Period, q.Kind)
		for _, p := range q.Also {
			if err != nil {
				break
			}
			var more []Bill
			more, err = s.between(ctx, userID, p, q.Kind)
			bills = append(bills, more...)
		}
	}
	if err != nil {
		return "", err
	}
	sum := sumOf(bills)
	if sum.Count == 0 && q.AllTime {
		return fmt.Sprintf("Нямам записани сметки%s.", kindLabel(q.Kind)), nil
	}
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
	fmt.Fprintf(&b, "Сметките%s за %s: общо %s (%s)%s.\n", kindLabel(q.Kind), q.Period.Label, Money(s.Cents, "EUR"), countWord(s.Count), foreignNote(s.Foreign))
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
	if q.AllTime {
		fmt.Fprintf(&b, "Всички записани сметки%s (%s, общо %s)%s:\n", kindLabel(q.Kind), countWord(s.Count), Money(s.Cents, "EUR"), foreignNote(s.Foreign))
	} else {
		fmt.Fprintf(&b, "Сметките%s за %s (%s, общо %s)%s:\n", kindLabel(q.Kind), q.Period.Label, countWord(s.Count), Money(s.Cents, "EUR"), foreignNote(s.Foreign))
	}
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
	cents, foreign := euroTotal(unpaid)
	fmt.Fprintf(b, "Неплатени от тях: %s (%s%s) — „кои не съм платил?“ ги изрежда.\n", countWord(len(unpaid)), Money(cents, "EUR"), foreignNote(foreign))
}

// unpaidText lists the unpaid bills the question asks for. Each has the number of its place among
// all the unpaid ones, the number "платих N" reads (pickBills): a list of one kind or one month is
// not numbered from 1 (on 2026-11-25 "платих 1" after the list of the electricity bills marked the
// first of all the unpaid ones).
func unpaidText(unpaid []Bill, q Question, today reminders.Day) string {
	var keep []Bill
	var place []int
	for i, u := range unpaid {
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
		place = append(place, i+1)
	}
	if len(keep) == 0 {
		return fmt.Sprintf("Нямаш неплатени сметки%s — всички записани са платени или се изтеглят автоматично (директен дебит).", kindLabel(q.Kind))
	}
	var b strings.Builder
	cents, foreign := euroTotal(keep)
	fmt.Fprintf(&b, "Неплатени%s: %s, общо %s%s:\n", kindLabel(q.Kind), countWord(len(keep)), Money(cents, "EUR"), foreignNote(foreign))
	for i, u := range keep {
		line := fmt.Sprintf("%d. %s", place[i], oneLine(u, nil))
		if u.DueDate != nil && dayOfTime(*u.DueDate).Before(today) {
			line += " ⚠️ срокът мина"
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "Когато платиш някоя: „платих %d“ или „платих тока“.", place[0])
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
	fmt.Fprintf(&s, "Сметките%s: %s — %s (%s)%s; %s — %s (%s)%s.\n", kindLabel(q.Kind),
		q.Period.Label, Money(a.Cents, "EUR"), countWord(a.Count), foreignNote(a.Foreign), q.Other.Label, Money(b.Cents, "EUR"), countWord(b.Count), foreignNote(b.Foreign))
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
