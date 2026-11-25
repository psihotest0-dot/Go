package bills

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/reminders"
)

func intp(n int) *int { return &n }

func TestReadCorrection(t *testing.T) {
	due := time.Date(2027, 9, 15, 0, 0, 0, 0, time.UTC)
	// The owner's answers of 2026-11-25 to the proposals they changed.
	for m, want := range map[string]Correction{
		"Има срок -следващата дата е 15.09.2027":             {Due: &due},
		"Да но я промени че следващото плащане е 87,00 евро": {Cents: 8700, Currency: "EUR"},
		"Една седмица по рано":                               {RemindBefore: intp(7)},
		"изтегля се автоматично,да":                          {Yes: true, Payment: DirectDebit},
		"за това плащане няма нужда от напомняне":            {RemindBefore: intp(-1)},
		"3 дни преди срока":                                  {RemindBefore: intp(3)},
		"напомни ми два дни по-рано":                         {RemindBefore: intp(2)},
		"напомни ми в самия ден":                             {RemindBefore: intp(0)},
		"превеждам я сам, срокът е 15.12.2026":               {Payment: Transfer, Due: ptrTime(time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC))},
		"да, без напомняне":                                  {Yes: true, RemindBefore: intp(-1)},
	} {
		got, ok := ReadCorrection(m)
		assert.True(t, ok, m)
		assert.Equal(t, want, got, m)
	}
	// The owner's of 2026-11-25, 17:51 and 17:54, with "нямам" and a typo.
	for _, m := range []string{"изтегля се автоматично,нямам нужда от запомняне", "автоматично е ,нямам нужда от напомняне", "не ми трябва напомняне"} {
		c, ok := ReadCorrection(m)
		assert.True(t, ok, m)
		require.NotNil(t, c.RemindBefore, m)
		assert.Equal(t, -1, *c.RemindBefore, m)
	}
	// "до" a day is the last day, not a due day (2026-11-25, 18:35).
	c, ok := ReadCorrection("Можеш ли да добавиш че е плащането е до 01.06.2028")
	assert.True(t, ok)
	assert.Nil(t, c.Due)
	require.NotNil(t, c.Until)
	assert.Equal(t, time.Date(2028, 6, 1, 0, 0, 0, 0, time.UTC), *c.Until)
	for _, m := range []string{"да", "не", "Малко по рано", "Благодаря", "добре"} {
		_, ok := ReadCorrection(m)
		assert.False(t, ok, m)
	}
	// "да" inside a request is no yes.
	c, _ = ReadCorrection("напомни ми да платя 3 дни преди срока")
	assert.False(t, c.Yes)
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestTheDaysOfTheReminder(t *testing.T) {
	loc := reminders.Location()
	due := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, loc)
	b := Bill{Issuer: "Beispiel", Kind: "insurance", Cents: 8700, Currency: "EUR", DueDate: &due, Payment: Transfer, RemindBefore: 7}
	assert.Equal(t, time.Date(2026, 12, 13, 9, 0, 0, 0, loc), PlanReminder(b, now, loc).At)

	// A week before has passed: the day before.
	late := time.Date(2026, 12, 15, 12, 0, 0, 0, loc)
	assert.Equal(t, time.Date(2026, 12, 19, 9, 0, 0, 0, loc), PlanReminder(b, late, loc).At)

	b.RemindBefore = 0
	assert.Equal(t, time.Date(2026, 12, 20, 9, 0, 0, 0, loc), PlanReminder(b, now, loc).At)

	b.RemindBefore = -1
	plan := PlanReminder(b, now, loc)
	assert.Empty(t, plan.Text)
	assert.Contains(t, plan.Why, "не искаш напомняне")
}

func TestTheDroppedValueIsSaid(t *testing.T) {
	b, problem := Verify(Fields{IsBill: true, Issuer: "Beispiel", Kind: "car", Amount: "47,18", InvoiceDate: "12.03.2026", Payment: Unknown, Repeat: "monthly"},
		"Beispiel GmbH\nZu zahlender Betrag 47,18 €")
	require.Empty(t, problem)
	text := ProposalText("a.pdf", b, nil, time.Date(2026, 11, 25, 12, 0, 0, 0, reminders.Location()), reminders.Location())
	assert.Contains(t, text, "Моделът даде датата на сметката „12.03.2026“, но я няма в текста")
}

func TestALoanIsAKindOfItsOwn(t *testing.T) {
	assert.Contains(t, Kinds, "loan")
	assert.Equal(t, "кредити и лизинг", KindNames["loan"])
	assert.Equal(t, "loan", standingKind("other", "запиши, че всеки месец на 15-ти плащам 300 € вноска по кредита"))
	assert.Contains(t, string(schema()), `"loan"`, "the model is offered it")
	assert.NotContains(t, extractPrompt, "loan", "and not told of it: told, it took every bill for a loan")

	// The credit statement of 2026-11-25, read as an insurance.
	b, problem := Verify(Fields{IsBill: true, Issuer: "Beispiel Kreditbank", Kind: "insurance", Amount: "47,18", Payment: DirectDebit, Repeat: "monthly",
		Summary: "Отчет за кредит"}, "Zu zahlender Betrag 47,18 €")
	require.Empty(t, problem)
	assert.Equal(t, "loan", b.Kind)
	b, _ = Verify(Fields{IsBill: true, Issuer: "Beispiel Versicherung", Kind: "insurance", Amount: "47,18", Payment: DirectDebit, Repeat: "yearly",
		Summary: "Застраховка, платима с кредитна карта"}, "Zu zahlender Betrag 47,18 €")
	assert.Equal(t, "insurance", b.Kind, "a credit card is a way to pay, not a loan")
}

func TestAListWithoutAPeriodIsOfAllTheBills(t *testing.T) {
	today := reminders.Day{Year: 2026, Month: 11, Day: 25}
	q, ok := ReadQuestion("какви сметки имам записани", today)
	require.True(t, ok)
	assert.True(t, q.AllTime)
	q, _ = ReadQuestion("какви сметки имам за ноември", today)
	assert.False(t, q.AllTime)
	q, _ = ReadQuestion("колко са сметките?", today)
	assert.False(t, q.AllTime, "a total is of this year")
}
