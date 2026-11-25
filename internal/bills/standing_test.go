package bills

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/reminders"
)

func TestReadStandingRequest(t *testing.T) {
	// The owner's words of 2026-11-25, with the typo of the name.
	r, ok := ReadStandingRequest("запише че на всяка 28 дата имам сметка от 14 евро към beispeil spiele club")
	assert.True(t, ok)
	assert.Equal(t, StandingRequest{Cents: 1400, Currency: "EUR", Day: 28, Payment: Unknown}, r)

	for m, want := range map[string]StandingRequest{
		"запиши, че всеки месец на 10-то число плащам 41,20 € за телефон, изтегля се автоматично": {Cents: 4120, Currency: "EUR", Day: 10, Payment: DirectDebit},
		"запиши че всеки 5-ти плащам наем 450 евро, превеждам го":                                 {Cents: 45000, Currency: "EUR", Day: 5, Payment: Transfer},
		"добави постоянна сметка: всеки месец на 1-во число 12,5 евро абонамент":                  {Cents: 1250, Currency: "EUR", Day: 1, Payment: Unknown},
		"запиши, че всяка година на 15 март плащам 120 € застраховка":                             {Cents: 12000, Currency: "EUR", Day: 15, Month: 3, Payment: Unknown},
		"запиши, че на 12 число всеки месец плащам 30 лв за интернет":                             {Cents: 3000, Currency: "BGN", Day: 12, Payment: Unknown},
		"I pay 9.99 EUR every month on the 3rd for a subscription, add it":                        {Cents: 999, Currency: "EUR", Day: 3, Payment: Unknown},
		// "на 12" is the amount, not the day.
		"запиши че плащам на 16 евро всеки месец на 26-ти за игри": {Cents: 1600, Currency: "EUR", Day: 26, Payment: Unknown},
	} {
		got, ok := ReadStandingRequest(m)
		assert.True(t, ok, m)
		assert.Equal(t, want, got, m)
	}

	// The day in a word (2026-11-25, 17:37), and "петнадесето" is not "пето".
	for m, day := range map[string]int{
		"добави към сметките :Наем - 640 евро-всяко първо число на месеца- автоматично изтегляне": 1,
		"запиши, че всяко петнадесето число на месеца плащам 30 € абонамент":                      15,
		"запиши, че всяко пето число плащам 30 € абонамент":                                       5,
		"запиши, че на последния ден всеки месец плащам 30 € наем":                                31,
	} {
		r, ok := ReadStandingRequest(m)
		assert.True(t, ok, m)
		assert.Equal(t, day, r.Day, m)
		assert.Empty(t, r.Missing, m)
	}
	r, ok = ReadStandingRequest("добави: наем 640 € на 1-во всеки месец; телефон 40 € на 10-то всеки месец")
	assert.True(t, ok)
	assert.Equal(t, 2, r.Several)

	// The last day (2026-11-25, 18:33): its digits are no day of the month.
	r, ok = ReadStandingRequest("Запиши че всеки месец на 1во число до 01.05.2028 включително ,плащам 240,00 евро лизинг за автомобил,автоматично ,без напомняне")
	assert.True(t, ok)
	require.NotNil(t, r.Until)
	assert.Equal(t, time.Date(2028, 5, 1, 0, 0, 0, 0, time.UTC), *r.Until)
	assert.Equal(t, 1, r.Day)
	assert.Equal(t, int64(24000), r.Cents)
	assert.Equal(t, DirectDebit, r.Payment)
	r, _ = ReadStandingRequest("запиши, че последното плащане е на 01.03.2027, всеки месец на 15-ти плащам 90 € вноска")
	require.NotNil(t, r.Until)
	assert.Equal(t, 15, r.Day, "not the 1 of the date")

	// Asked for, but not complete: the code says what is missing.
	r, ok = ReadStandingRequest("запиши, че всеки месец плащам абонамент за игри")
	assert.True(t, ok)
	assert.Equal(t, []string{"сумата", "деня от месеца"}, r.Missing)
	r, ok = ReadStandingRequest("запиши, че всяка година плащам 120 € застраховка на 15-ти")
	assert.True(t, ok)
	assert.Equal(t, []string{"месеца"}, r.Missing)

	for _, m := range []string{
		"колко плащам всеки месец за телефон?",            // a question
		"имам ли сметка от 16 евро всеки месец на 26-ти?", // a question too
		"кои сметки са месечни",                           // a question
		"за записа за телефон и интернет -може да го направиш като месечно плащане ,да речем всеки месец на 10-то число", // no request to keep (the owner's, 16:17)
		"напомни ми всеки месец на 5-ти да платя наема",                                                                  // a reminder: no amount, no "имам/запиши"
		"всеки ден пия кафе",                               // no bill
		"запиши във файл бележки.md: купих хляб за 2 евро", // not every month
	} {
		_, ok := ReadStandingRequest(m)
		assert.False(t, ok, m)
	}
}

func TestTheDaysOfAStandingBill(t *testing.T) {
	day := func(y int, m time.Month, d int) reminders.Day { return reminders.Day{Year: y, Month: m, Day: d} }
	games := Standing{Day: 28, Since: day(2026, 11, 25)}
	assert.Equal(t, day(2026, 11, 28), nextStandingDay(games, day(2026, 11, 25)))
	assert.Equal(t, day(2026, 12, 28), nextStandingDay(games, day(2026, 11, 29)))
	assert.Equal(t, []reminders.Day{day(2026, 11, 28), day(2026, 12, 28), day(2027, 1, 28)}, standingDays(games, day(2027, 1, 30)))

	// The 31st is the last day of a shorter month; the 10th, kept on the 25th, starts next month.
	last := Standing{Day: 31, Since: day(2027, 1, 31)}
	assert.Equal(t, []reminders.Day{day(2027, 1, 31), day(2027, 2, 28), day(2027, 3, 31)}, standingDays(last, day(2027, 3, 31)))
	phone := Standing{Day: 10, Since: day(2026, 11, 25)}
	assert.Equal(t, day(2026, 12, 10), nextStandingDay(phone, phone.Since))
	assert.Empty(t, standingDays(phone, day(2026, 12, 9)), "nothing before the day it was kept")

	// A last day: no day after it.
	until := day(2027, 2, 1)
	lease := Standing{Day: 1, Since: day(2026, 11, 25), Until: &until}
	assert.Equal(t, []reminders.Day{day(2026, 12, 1), day(2027, 1, 1), day(2027, 2, 1)}, standingDays(lease, day(2027, 6, 1)))
	assert.Equal(t, "всеки месец на 1-о число до 01.02.2027 включително", standingWhen(lease))

	yearly := Standing{Day: 15, Month: 3, Since: day(2026, 11, 25)}
	assert.Equal(t, day(2027, 3, 15), nextStandingDay(yearly, yearly.Since))
	assert.Equal(t, day(2028, 3, 15), nextStandingDay(yearly, day(2027, 3, 16)))
}

func TestTheIssuerAndTheKindOfAStandingBill(t *testing.T) {
	msg := "запише че на всяка 28 дата имам сметка от 14 евро към beispeil spiele club"
	assert.Equal(t, "Beispiel Spiele Club", standingIssuer("Beispiel Spiele Club", msg), "a word of it is in the message")
	assert.Equal(t, "", standingIssuer("Muster Kino", msg), "a name the message does not hold is not kept")
	assert.Equal(t, "subscription", standingKind("subscription", msg))
	assert.Equal(t, "phone_internet", standingKind("other", "всеки месец 20 € за интернет"), "the person's words win")
	assert.Equal(t, "other", standingKind("made_up", msg))
}

func TestAQuestionWhereTheBillsAreKept(t *testing.T) {
	today := reminders.Day{Year: 2026, Month: 11, Day: 25}
	for _, m := range []string{
		"сметките имат ли копие във базата данни или само в bils.txt?", // the owner's, 16:19
		"а къде записа сметките за интернет и телефон",                 // the owner's, 16:11
		"пазиш ли сметките в базата?",
		"where are my bills stored?",
	} {
		q, ok := ReadQuestion(m, today)
		assert.True(t, ok, m)
		assert.Equal(t, AskWhere, q.Ask, m)
	}
	for m, want := range map[string]string{
		"колко са сметките, записани този месец?": AskTotal,
		"кои сметки не съм платил?":               AskUnpaid,
		"какви сметки имаш записани":              AskList,
	} {
		q, ok := ReadQuestion(m, today)
		assert.True(t, ok, m)
		assert.Equal(t, want, q.Ask, m)
	}
}

// The standing bill a request to remove names (the code review of 2026-11-25): only by its number in
// the list or by its own words, never by the words of the request itself.
func TestTheStandingBillARequestToRemoveNames(t *testing.T) {
	two := []Standing{
		{ID: 3, Issuer: "Beispiel Wohnbau", Kind: "rent", Summary: "Наем на жилището", Day: 1},
		{ID: 4, Issuer: "Beispiel Spiele Club", Kind: "subscription", Summary: "Абонамент за игри", Day: 28},
	}
	one := two[1:]
	for _, c := range []struct {
		message string
		all     []Standing
		pick    []int
		named   bool
	}{
		{"махни постоянната сметка за наем", two, []int{0}, true},
		{"махни постоянната сметка за spiele club", two, []int{1}, true},
		{"махни абонамента за игри", two, []int{1}, true},
		{"махни постоянната сметка за наема", two, []int{0}, true}, // the kind in its own word
		{"махни постоянна 2", two, []int{1}, true},
		{"махни постоянната сметка 1", two, []int{0}, true},
		{"махни абонамент №2", two, []int{1}, true},
		// The words of the request alone name none: the code asks which.
		{"махни абонамента", one, nil, false},
		{"махни постоянната сметка", two, nil, false},
		{"махни постоянна 7", two, nil, false},
		// Words that are no standing bill's: none, even when there is one bill only.
		{"спри абонамента за Netflix", one, nil, true},
		{"спри абонамента за Netflix", two, nil, true},
		{"махни месечната сметка за тока", two, nil, true},
		// A number that is no number of the list.
		{"махни абонамента за 5G интернета", []Standing{two[0], two[1], two[0], two[1], two[0]}, nil, true},
	} {
		pick, named := pickStanding(c.all, c.message)
		assert.Equal(t, c.pick, pick, c.message)
		assert.Equal(t, c.named, named, c.message)
	}
}
