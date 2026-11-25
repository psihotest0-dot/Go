package bills

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"go-monolith/internal/db"
	"go-monolith/internal/reminders"
	"go-monolith/metrics"
)

// The answer to a proposal can change it (the owner, 2026-11-25). In the Flet chat of that evening
// "Има срок - следващата дата е 15.09.2027", "промени я, че следващото плащане е 87,00 евро",
// "една седмица по-рано" and "изтегля се автоматично, да" each dropped the proposal they answered,
// as anything but a bare "да" or "не" did, and the model went on as if it had done them: "добавям
// датата…", "ще те напомня точно една седмица преди…", "Записах в базата…". Nothing was kept.
//
// Now the code reads such an answer (ReadCorrection): a due day, an amount, the way it is paid, the
// days of the reminder ("седмица по-рано", "3 дни преди", "в деня", "без напомняне") and whether it
// also says yes. The proposal is changed and said back; with a yes in it, it is kept at once.

// correctionMaxRunes: an answer to a proposal is a sentence or two.
const correctionMaxRunes = 240

// Correction is what an answer to a proposal changes. Nil and zero fields change nothing.
type Correction struct {
	Yes          bool
	Due          *time.Time
	Cents        int64
	Currency     string
	Payment      string
	RemindBefore *int
}

var (
	// correctionYes: "да" at the start or the end of the answer, apart from the rest ("изтегля се
	// автоматично, да"); "напомни ми да платя" is not one.
	correctionYes = regexp.MustCompile(`(?i)(?:^\s*(?:да|добре|ок|окей|ok|yes|запиши\s+(?:я|го))(?:\s*[,.!;]|\s*$)|[,.!;]\s*(?:да|добре|ок|окей|ok|yes|запиши\s+(?:я|го))\s*[.!]*\s*$)`)
	// correctionNoReminder: "без напомняне", "няма нужда от напомняне", "не ми напомняй".
	correctionNoReminder = regexp.MustCompile(`(?i)(?:без\s+напомн|няма\s+нужда\s+от\s+напомн|не\s+(?:ми\s+)?напомняй|не\s+искам\s+напомн|не\s+ми\s+трябва\s+напомн|no\s+reminder)`)
	// correctionLead: "седмица по-рано", "3 дни преди", "два дни преди срока", "един месец по-рано".
	correctionLead = regexp.MustCompile(`(?i)(?:^|[^\p{L}\d])(\d{1,2}|един|една|два|две|три|четири|пет)?\s*(ден|дни|дена|седмиц\p{L}*|месец\p{L}*)\s+(?:по-?\s*рано|преди)`)
	// correctionSameDay: "в деня", "в самия ден".
	correctionSameDay = regexp.MustCompile(`(?i)(?:в\s+(?:самия\s+)?ден(?:я)?\s+(?:на\s+)?(?:срока|плащането)|в\s+самия\s+ден|on\s+the\s+day)`)
	// correctionDate: "15.09.2027", "1.7.27".
	correctionDate = regexp.MustCompile(`(?:^|[^\d.])(\d{1,2}\.\d{1,2}\.(?:\d{4}|\d{2}))(?:[^\d]|$)`)
)

var leadNumbers = map[string]int{"един": 1, "една": 1, "два": 2, "две": 2, "три": 3, "четири": 4, "пет": 5}

// ReadCorrection reads what an answer to a proposal changes; ok is false when it changes nothing
// (a bare "да" or "не" is not a correction).
func ReadCorrection(message string) (c Correction, ok bool) {
	m := strings.TrimSpace(message)
	if m == "" || utf8.RuneCountInString(m) > correctionMaxRunes {
		return c, false
	}
	changed := false
	if correctionNoReminder.MatchString(m) {
		none := -1
		c.RemindBefore, changed = &none, true
	} else if correctionSameDay.MatchString(m) {
		zero := 0
		c.RemindBefore, changed = &zero, true
	} else if l := correctionLead.FindStringSubmatch(m); l != nil {
		n := 1
		if l[1] != "" {
			if v, err := strconv.Atoi(l[1]); err == nil {
				n = v
			} else {
				n = leadNumbers[strings.ToLower(l[1])]
			}
		}
		unit := strings.ToLower(l[2])
		switch {
		case strings.HasPrefix(unit, "седмиц"):
			n *= 7
		case strings.HasPrefix(unit, "месец"):
			n *= 30
		}
		if n >= 1 && n <= 60 {
			c.RemindBefore, changed = &n, true
		}
	}
	if d := correctionDate.FindStringSubmatch(m); d != nil {
		if t, okD := parseDate(d[1]); okD {
			c.Due, changed = &t, true
		}
	}
	if a := standingAmount.FindStringSubmatchIndex(m); a != nil {
		num, cur := a[2:4], a[4:6]
		if a[2] < 0 {
			num, cur = a[8:10], a[6:8]
		}
		amount := m[num[0]:num[1]]
		if oneDecimal.MatchString(amount) {
			amount += "0"
		}
		if cents, okA := parseAmount(amount); okA && cents > 0 {
			c.Cents, c.Currency, changed = cents, currencyOf(m[cur[0]:cur[1]]), true
		}
	}
	switch {
	case standingDebitWords.MatchString(m):
		c.Payment, changed = DirectDebit, true
	case standingTransferWords.MatchString(m):
		c.Payment, changed = Transfer, true
	}
	c.Yes = correctionYes.MatchString(m)
	return c, changed
}

// what is the list of the changes, for the answer: "срок петък, 15.09.2027, сума 87,00 €".
func (c Correction) what() string {
	var parts []string
	if c.Due != nil {
		parts = append(parts, "срок "+dayText(*c.Due))
	}
	if c.Cents > 0 {
		parts = append(parts, "сума "+Money(c.Cents, c.Currency))
	}
	if c.Payment != "" {
		parts = append(parts, paymentText[c.Payment])
	}
	if c.RemindBefore != nil {
		parts = append(parts, remindWhen(*c.RemindBefore, "срока"))
	}
	return strings.Join(parts, ", ")
}

// CorrectBill changes the bill that waits in the conversation by the person's answer and says it
// back; with a yes in the answer, it is kept at once.
func (s *Service) CorrectBill(ctx context.Context, userID uuid.UUID, session string, p Bill, c Correction) string {
	if c.Due != nil {
		p.DueDate = c.Due
	}
	if c.Cents > 0 {
		p.Cents = c.Cents
		if c.Currency != "" {
			p.Currency = c.Currency
		}
	}
	if c.Payment != "" {
		p.Payment = c.Payment
	}
	if c.RemindBefore != nil {
		p.RemindBefore = *c.RemindBefore
	}
	row, err := s.q.UpdateProposedBill(ctx, db.UpdateProposedBillParams{ID: p.ID, UserID: userID, DueDate: nullDate(p.DueDate),
		AmountCents: p.Cents, Payment: p.Payment, RemindDaysBefore: int32(p.RemindBefore)})
	if errors.Is(err, sql.ErrNoRows) {
		return "Тази сметка вече е записана (или отказана)."
	}
	if err != nil {
		slog.WarnContext(ctx, "a proposed bill could not be changed", "err", err)
		return "Не успях да поправя сметката (грешка в базата). Нищо не е записано. Опитай пак след малко."
	}
	metrics.Bills.WithLabelValues("corrected").Inc()
	b := fromRow(row)
	if c.Yes {
		return "Поправих по твоите думи: " + c.what() + ".\n" + s.Keep(ctx, userID, session, b)
	}
	return "Поправих по твоите думи: " + c.what() + ". Още не е записана.\n\n" + ProposalText(b.FileName, b, nil, s.now(), s.loc)
}

// CorrectStanding changes the standing bill that waits in the conversation by the person's answer;
// with a yes in it, it is kept at once. A due day is no part of a standing bill.
func (s *Service) CorrectStanding(ctx context.Context, userID uuid.UUID, session string, p Standing, c Correction) string {
	if c.Due != nil {
		// "на 15.09.2027" for a yearly bill: its day and month.
		p.Day = c.Due.Day()
		if p.Month != 0 {
			p.Month = int(c.Due.Month())
		}
	}
	if c.Cents > 0 {
		p.Cents = c.Cents
	}
	if c.Payment != "" {
		p.Payment = c.Payment
	}
	if c.RemindBefore != nil {
		p.RemindBefore = *c.RemindBefore
	}
	row, err := s.q.UpdateProposedStanding(ctx, db.UpdateProposedStandingParams{ID: p.ID, UserID: userID, AmountCents: p.Cents,
		Payment: p.Payment, RemindDaysBefore: int32(p.RemindBefore), Day: int32(p.Day), Month: int32(p.Month)})
	if errors.Is(err, sql.ErrNoRows) {
		return "Тази сметка вече е записана (или отказана)."
	}
	if err != nil {
		slog.WarnContext(ctx, "a proposed standing bill could not be changed", "err", err)
		return "Не успях да поправя сметката (грешка в базата). Нищо не е записано. Опитай пак след малко."
	}
	metrics.Bills.WithLabelValues("standing_corrected").Inc()
	st := standingFromRow(row, s.loc)
	what := c.what()
	if c.Due != nil {
		what = strings.TrimPrefix(strings.Replace(what, "срок "+dayText(*c.Due), "денят "+strconv.Itoa(p.Day), 1), ", ")
	}
	if c.Yes {
		return "Поправих по твоите думи: " + what + ".\n" + s.KeepStanding(ctx, userID, session, st)
	}
	today := reminders.DayOf(s.now(), s.loc)
	return "Поправих по твоите думи: " + what + ". Още не е записана.\n\n" + standingProposalText(st, today, s.reminders != nil)
}
