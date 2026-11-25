package bills

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/reminders"
)

// A made-up bill in the shape of a German phone bill (no real company, person or account).
const phoneBill = `Beispiel Telefon GmbH
Rechnungsdatum 12.10.2026
Leistungszeitraum 08.09.2026 - 07.10.2026
Max Muster                                Fällig am 19.10.2026
Der zu zahlende Betrag wird am 19.10.2026 von dem Konto abgebucht.
Rechnungsbetrag 47,17 €
Zu zahlender Betrag 47,18 €
Gesamtbetrag aus vorheriger Rechnung 52,40`

func TestTheAmountAndTheDatesMustBeInTheText(t *testing.T) {
	f := Fields{IsBill: true, Issuer: "Beispiel Telefon", Kind: "phone_internet", Amount: "47,18", Currency: "€",
		InvoiceDate: "12.10.2026", DueDate: "19.10.2026", Payment: DirectDebit, Repeat: "monthly", Summary: "Телефон, октомври 2026"}
	b, problem := Verify(f, phoneBill)
	require.Empty(t, problem)
	assert.Equal(t, int64(4718), b.Cents)
	assert.Equal(t, "EUR", b.Currency)
	require.NotNil(t, b.DueDate)
	assert.Equal(t, "2026-10-19", b.DueDate.Format("2006-01-02"))
	assert.Empty(t, b.Dropped)

	// A made-up amount is no bill; a made-up date is dropped.
	f2 := f
	f2.Amount = "95,34"
	_, problem = Verify(f2, phoneBill)
	assert.Equal(t, AmountMissing, problem)
	f3 := f
	f3.Amount = "9,34" // a part of 47,18 is not the amount
	_, problem = Verify(f3, phoneBill)
	assert.Equal(t, AmountMissing, problem)
	f4 := f
	f4.DueDate = "20.10.2026"
	b, problem = Verify(f4, phoneBill)
	require.Empty(t, problem)
	assert.Nil(t, b.DueDate)
	assert.Equal(t, []string{"due_date"}, b.Dropped)
}

func TestAmountsAndDatesInTheirShapes(t *testing.T) {
	for in, want := range map[string]int64{"47,18": 4718, "1.234,50": 123450, "23.90": 2390, "85": 8500, "86,40 €": 8640} {
		got, ok := parseAmount(in)
		require.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	_, ok := parseAmount("abc")
	assert.False(t, ok)
	d, ok := parseDate("21.05.25")
	require.True(t, ok)
	assert.True(t, dateInText(d, "bis zum 21.05.25 auf das Konto"))
	d, ok = parseDate("7. Mai 2025")
	require.True(t, ok)
	assert.True(t, dateInText(d, "Datum: 7. Mai 2025"))
	_, ok = parseDate("31.02.2026")
	assert.False(t, ok)
}

func TestAPaidReceiptIsPaid(t *testing.T) {
	text := "Rechnungsbetrag in Höhe von 86,40 € bis zum 21.05.25. Wir haben den Betrag am 07.05.2025 dankend erhalten."
	b, problem := Verify(Fields{IsBill: true, Issuer: "Praxis", Kind: "doctor", Amount: "86,40", PaidDate: "07.05.2025", Payment: Transfer, Repeat: "once"}, text)
	require.Empty(t, problem)
	assert.Equal(t, Paid, b.Payment)
	plan := PlanReminder(b, time.Now(), reminders.Location())
	assert.Empty(t, plan.Text)
	assert.Contains(t, plan.Why, "платена")
}

func TestTheReminderOfADueDay(t *testing.T) {
	loc := reminders.Location()
	due := time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 24, 21, 0, 0, 0, loc)
	debit := Bill{Issuer: "Beispiel", Kind: "phone_internet", Cents: 4718, Currency: "EUR", DueDate: &due, Payment: DirectDebit, Summary: "Телефон", RemindBefore: 1}
	plan := PlanReminder(debit, now, loc)
	assert.Equal(t, time.Date(2026, 12, 4, 9, 0, 0, 0, loc), plan.At, "09:00 the day before")
	assert.Contains(t, plan.Text, "ще изтегли 47,18 €")
	transfer := debit
	transfer.Payment = Transfer
	assert.Contains(t, PlanReminder(transfer, now, loc).Text, "да платиш 47,18 € на Beispiel")
	// The day before has passed: 09:00 of the day itself; the day has passed: none.
	assert.Equal(t, time.Date(2026, 12, 5, 9, 0, 0, 0, loc), PlanReminder(debit, time.Date(2026, 12, 4, 10, 0, 0, 0, loc), loc).At)
	late := PlanReminder(debit, time.Date(2026, 12, 6, 10, 0, 0, 0, loc), loc)
	assert.Empty(t, late.Text)
	assert.Contains(t, late.Why, "мина")
}

func TestTheProposalSaysWhatWasRead(t *testing.T) {
	loc := reminders.Location()
	due := time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)
	b := Bill{Issuer: "Beispiel", Kind: "phone_internet", Cents: 123450, Currency: "EUR", DueDate: &due, Payment: DirectDebit, Repeat: "monthly", Summary: "Телефон", Dropped: []string{"invoice_date"}, RemindBefore: 1}
	text := ProposalText("rechnung.pdf", b, []Bill{{Issuer: "Beispiel", Cents: 123450, Currency: "EUR", FileName: "old.pdf"}}, time.Date(2026, 11, 24, 21, 0, 0, 0, loc), loc)
	assert.Contains(t, text, "Сума: 1 234,50 €")
	assert.Contains(t, text, "Срок: събота, 05.12.2026 — изтегля се автоматично")
	assert.Contains(t, text, "Месечна")
	assert.Contains(t, text, "датата на сметката, но я няма в текста")
	assert.Contains(t, text, "Прилича на вече записана сметка")
	assert.Contains(t, text, "Да я запиша ли? Ще ти напомня в петък, 04.12.2026, в 09:00")
}
