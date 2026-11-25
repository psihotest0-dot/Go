package bills

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go-monolith/internal/reminders"
)

// The sums are in euro (the code review of 2026-11-25: every sum was added up as euro, whatever its
// currency). A lev is at its fixed rate; another currency is left out and named.
func TestTheSumsAreInEuro(t *testing.T) {
	c, ok := euroCents(19558, "BGN")
	assert.True(t, ok)
	assert.Equal(t, int64(10000), c, "195,58 лв are 100 €")
	c, ok = euroCents(4718, "EUR")
	assert.True(t, ok)
	assert.Equal(t, int64(4718), c)
	_, ok = euroCents(5000, "USD")
	assert.False(t, ok)

	due := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	bills := []Bill{
		{Issuer: "Beispiel Strom", Kind: "electricity", Cents: 8000, Currency: "EUR", DueDate: &due, Payment: Transfer},
		{Issuer: "Наем София", Kind: "rent", Cents: 19558, Currency: "BGN", DueDate: &due, Payment: Transfer},
		{Issuer: "Example Inc", Kind: "subscription", Cents: 1200, Currency: "USD", DueDate: &due, Payment: Transfer},
	}
	s := sumOf(bills)
	assert.Equal(t, int64(18000), s.Cents, "80 € + 100 € from the lev; the dollars are not added")
	assert.Equal(t, 2, s.Count)
	assert.Equal(t, 1, s.Foreign)
	assert.Equal(t, int64(10000), s.ByKind["rent"])

	month := monthPeriod(2026, time.November)
	text := totalText(Question{Ask: AskTotal, Period: month}, s)
	assert.Contains(t, text, "общо 180,00 € (2 сметки) (+ 1 сметка в друга валута — не е в сбора).")
	assert.Contains(t, text, "Неплатени от тях: 3 сметки (180,00 € (+ 1 сметка в друга валута — не е в сбора))")
	report := ReportText(month, s, monthPeriod(2026, time.October), Sum{ByKind: map[string]int64{}}, bills, reminders.Day{Year: 2026, Month: 12, Day: 1})
	assert.Contains(t, report, "Сметките за ноември 2026: общо 180,00 € (2 сметки) (+ 1 сметка в друга валута — не е в сбора).")
	assert.Contains(t, report, "Неплатени сега: 3 сметки, 180,00 € (+ 1 сметка в друга валута — не е в сбора):")
	assert.Contains(t, report, "12,00 USD", "the bill itself keeps its currency")

	line := newRepeatLine("Example Inc — 12,00 USD", 1200, "USD")
	assert.Zero(t, line.cents)
	assert.Contains(t, line.text, "друга валута, не е в сбора")
}
