package bills

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/reminders"
)

func TestTheQuestionsAboutTheBills(t *testing.T) {
	today := reminders.Day{Year: 2026, Month: 11, Day: 24}
	cases := []struct {
		msg, ask, kind, period, other string
	}{
		{"колко платих за ток тази година?", AskTotal, "electricity", "2026 г.", ""},
		{"Колко са ми сметките този месец?", AskTotal, "", "ноември 2026", ""},
		{"колко харчих за телефон миналия месец", AskTotal, "phone_internet", "октомври 2026", ""},
		{"кои сметки не съм платил?", AskUnpaid, "", "", ""},
		{"какви сметки имам за октомври", AskList, "", "октомври 2026", ""},
		{"сравни сметките за ноември с октомври", AskCompare, "", "ноември 2026", "октомври 2026"},
		{"сравни разходите с миналия месец", AskCompare, "", "ноември 2026", "октомври 2026"},
		{"колко платих за застраховки през 2025", AskTotal, "insurance", "2025 г.", ""},
		{"колко струваха сметките през декември", AskTotal, "", "декември 2025", ""},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			q, ok := ReadQuestion(c.msg, today)
			require.True(t, ok)
			assert.Equal(t, c.ask, q.Ask)
			assert.Equal(t, c.kind, q.Kind)
			assert.Equal(t, c.period, q.Period.Label)
			if c.other != "" {
				require.NotNil(t, q.Other)
				assert.Equal(t, c.other, q.Other.Label)
			}
		})
	}
	for _, no := range []string{"платих тока", "колко струва токът в Германия?", "как си?", "колко е часът"} {
		_, ok := ReadQuestion(no, today)
		assert.False(t, ok, no)
	}
}

// The code review of 2026-11-27: "за януари и февруари" was answered for January alone.
func TestATotalOfSeveralMonthsHasThemAll(t *testing.T) {
	today := reminders.Day{Year: 2026, Month: 11, Day: 24}
	day := func(m time.Month, d int) reminders.Day { return reminders.Day{Year: 2026, Month: m, Day: d} }
	cases := []struct {
		msg, label string
		from, to   reminders.Day
		also       []string
	}{
		{"колко платих за януари и февруари", "януари – февруари 2026", day(1, 1), day(2, 28), nil},
		{"колко платих от януари до март", "януари – март 2026", day(1, 1), day(3, 31), nil},
		{"колко платих за януари и март", "януари 2026 и март 2026", day(1, 1), day(1, 31), []string{"март 2026"}},
		{"какви сметки имам за март, януари и февруари", "януари – март 2026", day(1, 1), day(3, 31), nil},
		{"колко платих за май, май и юли", "май 2026 и юли 2026", day(5, 1), day(5, 31), []string{"юли 2026"}},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			q, ok := ReadQuestion(c.msg, today)
			require.True(t, ok)
			assert.Equal(t, c.label, q.Period.Label)
			assert.Equal(t, c.from, q.Period.From)
			assert.Equal(t, c.to, q.Period.To)
			var also []string
			for _, p := range q.Also {
				also = append(also, monthsLabel(p))
			}
			assert.Equal(t, c.also, also)
		})
	}
	q, _ := ReadQuestion("колко платих за ноември", today)
	assert.Empty(t, q.Also, "one month is one period, as before")
	assert.Equal(t, "ноември 2026", q.Period.Label)
}

func TestTheSumsAndTheComparison(t *testing.T) {
	d := func(y int, m time.Month, day int) *time.Time {
		t := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
		return &t
	}
	nov := []Bill{
		{Issuer: "A", Kind: "phone_internet", Cents: 4718, DueDate: d(2026, 11, 19), Payment: DirectDebit},
		{Issuer: "B", Kind: "electricity", Cents: 8000, DueDate: d(2026, 11, 28), Payment: Transfer},
	}
	oct := []Bill{{Issuer: "A", Kind: "phone_internet", Cents: 5240, DueDate: d(2026, 10, 19), Payment: DirectDebit}}
	q := Question{Ask: AskTotal, Period: monthPeriod(2026, 11)}
	text := totalText(q, sumOf(nov))
	assert.Contains(t, text, "общо 127,18 € (2 сметки)")
	assert.Contains(t, text, "• ток — 80,00 € (1 сметка)")
	assert.Contains(t, text, "Неплатени от тях: 1 сметка (80,00 €)")
	other := monthPeriod(2026, 10)
	cmp := compareText(Question{Ask: AskCompare, Period: monthPeriod(2026, 11), Other: &other}, sumOf(nov), sumOf(oct))
	assert.Contains(t, cmp, "ноември 2026 — 127,18 € (2 сметки); октомври 2026 — 52,40 € (1 сметка)")
	assert.Contains(t, cmp, "Разлика: +74,78 € (+142 %)")
	assert.Contains(t, cmp, "• телефон и интернет: 47,18 € — 52,40 € (−5,22 € (−9 %))")
	assert.Contains(t, cmp, "💡 Най-много поевтиня телефон и интернет: −5,22 € (−9 %).")
	assert.Contains(t, text, "💡 Най-голямото перо е ток — 80,00 € (62 % от всичко).")
	report := ReportText(monthPeriod(2026, 10), sumOf(oct), monthPeriod(2026, 9), Sum{}, []Bill{nov[1]}, reminders.Day{Year: 2026, Month: 11, Day: 1})
	assert.Contains(t, report, "📊 Сметките за октомври 2026: общо 52,40 € (1 сметка)")
	assert.Contains(t, report, "Неплатени сега: 1 сметка, 80,00 €")
}

// "Продължи" has "дълж" inside it: every "продължи" was taken for a question about the unpaid
// bills and answered by the code (the live check of 2026-11-28).
func TestAWordWithTheStemOfOwingInsideItIsNoQuestionAboutTheBills(t *testing.T) {
	today := reminders.Day{Year: 2026, Month: 11, Day: 28}
	for _, m := range []string{"продължи", "продължавай", "продължи автоматично за цялата информация", "издължено ли е въжето?"} {
		_, ok := ReadQuestion(m, today)
		assert.False(t, ok, m)
	}
	for _, m := range []string{"какво дължа?", "колко дължа за ток", "какви задължения имам?"} {
		q, ok := ReadQuestion(m, today)
		assert.True(t, ok, m)
		assert.Equal(t, AskUnpaid, q.Ask, m)
	}
}
