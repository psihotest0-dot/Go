package bills

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"go-monolith/internal/reminders"
)

// reminderMinute is the hour of the reminder of a due day: 09:00 of the day before.
const reminderMinute = 9 * 60

// Money is "47,18 €" ("1 234,50 €" for a larger sum; another currency by its code).
func Money(cents int64, currency string) string {
	whole, frac := cents/100, cents%100
	s := fmt.Sprintf("%d", whole)
	if len(s) > 3 {
		var b strings.Builder
		for i, r := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				b.WriteRune(' ')
			}
			b.WriteRune(r)
		}
		s = b.String()
	}
	sign := "€"
	if currency != "" && currency != "EUR" {
		sign = currency
	}
	return fmt.Sprintf("%s,%02d %s", s, frac, sign)
}

func dayText(t time.Time) string {
	return reminders.DateText(reminders.Day{Year: t.Year(), Month: t.Month(), Day: t.Day()})
}

// title is "Beispiel (телефон и интернет)".
func title(b Bill) string {
	kind := KindNames[b.Kind]
	if b.Issuer == "" {
		return kind
	}
	if kind == "" || kind == KindNames["other"] {
		return b.Issuer
	}
	return fmt.Sprintf("%s (%s)", b.Issuer, kind)
}

// oneLine is "Beispiel (телефон и интернет), 47,18 €, срок петък, 19.10.2026".
func oneLine(b Bill, _ *time.Location) string {
	s := title(b) + ", " + Money(b.Cents, b.Currency)
	switch {
	case b.PaidDate != nil:
		s += ", платена на " + dayText(*b.PaidDate)
	case b.DueDate != nil:
		s += ", срок " + dayText(*b.DueDate)
	case b.InvoiceDate != nil:
		s += ", от " + dayText(*b.InvoiceDate)
	}
	return s
}

var paymentText = map[string]string{
	DirectDebit: "изтегля се автоматично от сметката ти (директен дебит)",
	Transfer:    "трябва да я платиш сам (превод)",
	Paid:        "вече е платена",
	Unknown:     "не пише как се плаща",
}

var repeatText = map[string]string{
	"monthly": "месечна", "quarterly": "на всеки три месеца", "yearly": "годишна", "once": "еднократна",
}

var droppedText = map[string]string{
	"due_date": "срока", "paid_date": "датата на плащане", "invoice_date": "датата на сметката",
}

// ReminderPlan is the reminder a kept bill gets: Text and At, or Why there is none.
type ReminderPlan struct {
	Text string
	At   time.Time
	Why  string
}

// PlanReminder is the reminder of a bill's due day: 09:00 RemindBefore days before it (the day
// before unless the person said otherwise), else the day before, else the day itself, whichever is
// still to come; none for a bill that is paid, has no due day, whose day has passed, or that the
// person wants none for.
func PlanReminder(b Bill, now time.Time, loc *time.Location) ReminderPlan {
	if b.Payment == Paid || b.PaidDate != nil {
		return ReminderPlan{Why: "сметката вече е платена"}
	}
	if b.RemindBefore < 0 {
		return ReminderPlan{Why: "не искаш напомняне за нея"}
	}
	if b.DueDate == nil {
		return ReminderPlan{Why: "в документа няма срок; ако искаш, кажи ми „напомни ми на … да платя …“"}
	}
	due := reminders.Day{Year: b.DueDate.Year(), Month: b.DueDate.Month(), Day: b.DueDate.Day()}
	var at time.Time
	// The days the person asked for first, then the day before and the day itself; a day that comes
	// earlier than the first has passed when the first has.
	for _, days := range []int{b.RemindBefore, 1, 0} {
		if at = due.AddDays(-days).Time(reminderMinute, loc); at.After(now) {
			break
		}
	}
	if !at.After(now) {
		return ReminderPlan{Why: "срокът (" + reminders.DateText(due) + ") вече мина"}
	}
	money := Money(b.Cents, b.Currency)
	what := b.Summary
	if what == "" {
		what = KindNames[b.Kind]
	}
	var text string
	switch b.Payment {
	case DirectDebit:
		text = fmt.Sprintf("%s %s ще изтегли %s от сметката ти (%s) — да има пари", reminders.OnDateText(due), b.Issuer, money, what)
	case Transfer:
		text = fmt.Sprintf("да платиш %s на %s (%s) — срокът е %s", money, b.Issuer, what, reminders.DateText(due))
	default:
		text = fmt.Sprintf("срок за плащане %s: %s на %s (%s)", reminders.DateText(due), money, b.Issuer, what)
	}
	return ReminderPlan{Text: text, At: at}
}

// ProposalText is what the code says about a bill it read, with the question whether to keep it.
// same are kept bills that look like this one.
func ProposalText(name string, b Bill, same []Bill, now time.Time, loc *time.Location) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Прочетох сметката „%s“:\n", name)
	fmt.Fprintf(&s, "• %s", title(b))
	if b.Summary != "" {
		fmt.Fprintf(&s, " — %s", b.Summary)
	}
	fmt.Fprintf(&s, "\n• Сума: %s\n", Money(b.Cents, b.Currency))
	switch {
	case b.PaidDate != nil:
		fmt.Fprintf(&s, "• Платена на %s\n", dayText(*b.PaidDate))
	case b.DueDate != nil:
		fmt.Fprintf(&s, "• Срок: %s — %s\n", dayText(*b.DueDate), paymentText[b.Payment])
	default:
		fmt.Fprintf(&s, "• Без срок в документа — %s\n", paymentText[b.Payment])
	}
	if b.InvoiceDate != nil {
		fmt.Fprintf(&s, "• Дата на сметката: %s\n", dayText(*b.InvoiceDate))
	}
	if r := repeatText[b.Repeat]; r != "" {
		fmt.Fprintf(&s, "• %s\n", capitalize(r))
	}
	for _, d := range b.Dropped {
		if v := b.DroppedValues[d]; v != "" {
			fmt.Fprintf(&s, "• (Моделът даде %s „%s“, но я няма в текста — не я пиша.)\n", droppedText[d], v)
		} else {
			fmt.Fprintf(&s, "• (Моделът даде и %s, но я няма в текста — не я пиша.)\n", droppedText[d])
		}
	}
	for _, o := range same {
		fmt.Fprintf(&s, "⚠️ Прилича на вече записана сметка: %s (файл „%s“) — може да е друга страница или копие.\n", oneLine(o, loc), o.FileName)
	}
	plan := PlanReminder(b, now, loc)
	s.WriteString("\n")
	if plan.Text != "" {
		fmt.Fprintf(&s, "Да я запиша ли? Ще ти напомня %s: „%s“.", reminders.OnText(plan.At, now, loc), plan.Text)
	} else {
		fmt.Fprintf(&s, "Да я запиша ли? (Без напомняне: %s.)", plan.Why)
	}
	s.WriteString(" Кажи „да“ или „не“.")
	return s.String()
}

func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

var listNumberRe = regexp.MustCompile(`(?:^|[^\d])(\d{1,2})(?:[^\d,.]|$)`)

// stopStems are the words of "платих сметката за …" that name no bill.
var stopStems = map[string]bool{"плати": true, "сметк": true, "факту": true, "вече": true, "днес": true, "вчера": true, "преве": true, "изпла": true}

// stems are the first five letters of the words of three letters or more.
func stems(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) }) {
		rs := []rune(w)
		if len(rs) < 3 {
			continue
		}
		st := string(rs[:min(5, len(rs))])
		if !stopStems[st] {
			out[st] = true
		}
	}
	return out
}
