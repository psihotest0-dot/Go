package agent

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/bills"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/reminders"
)

// fakeText is a reader of files that gives one text.
type fakeText struct{ text string }

func (f fakeText) Extract(context.Context, io.Reader) (string, error) { return f.text, nil }

// A made-up bill (no real company, person or account).
const madeUpBill = `Beispiel Telefon GmbH
Rechnungsdatum 20.11.2026
Fällig am 05.12.2026 - der Betrag wird von Ihrem Konto abgebucht.
Zu zahlender Betrag 47,18 €`

type billEnv struct {
	*reminderEnv
	svc   *bills.Service
	model *llm.MockLLM
}

func newBillEnv(t *testing.T) *billEnv {
	t.Helper()
	e := newReminderEnv(t)
	model := &llm.MockLLM{}
	svc := bills.NewService(e.db, fakeText{madeUpBill}, fakeText{madeUpBill}, bills.NewExtractor(model), e.store, func() { e.wakes++ })
	bills.SetClock(svc, func() time.Time { return reminderNow })
	e.o.WithBills(svc)
	return &billEnv{reminderEnv: e, svc: svc, model: model}
}

func (e *billEnv) reads(t *testing.T, f bills.Fields) {
	t.Helper()
	raw, err := json.Marshal(f)
	require.NoError(t, err)
	e.model.Response = openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: string(raw)}}}}
}

var madeUpFields = bills.Fields{IsBill: true, Issuer: "Beispiel Telefon", Kind: "phone_internet", Amount: "47,18", Currency: "€",
	InvoiceDate: "20.11.2026", DueDate: "05.12.2026", Payment: bills.DirectDebit, Repeat: "monthly", Summary: "Телефон, ноември 2026"}

func TestABillIsReadSaidBackAndKeptWithItsReminder(t *testing.T) {
	e := newBillEnv(t)
	e.reads(t, madeUpFields)
	out, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	assert.Contains(t, out, "Сума: 47,18 €")
	assert.Contains(t, out, "Срок: събота, 05.12.2026 — изтегля се автоматично")
	assert.Contains(t, out, "Ще ти напомня в петък, 04.12.2026, в 09:00")

	msgs, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: e.session, Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs, 2, "the upload and the answer are in the conversation")

	reply := e.say(t, "да")
	assert.Contains(t, reply, "Записах сметката ✅")
	assert.Contains(t, reply, "Ще ти напомня")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
	active, err := e.store.Active(context.Background(), e.userID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Contains(t, active[0].Text, "ще изтегли 47,18 €")
	assert.Equal(t, time.Date(2026, 12, 4, 9, 0, 0, 0, reminders.Location()), active[0].Next.In(reminders.Location()))

	// The same file again is not read again; another file with the same bill is flagged.
	out, _ = e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	assert.Contains(t, out, "вече е записана (същият файл)")
	out, _ = e.svc.Read(context.Background(), e.userID, e.session, "kopie.pdf", []byte("%PDF-1.4 two"))
	assert.Contains(t, out, "Прилича на вече записана сметка")
	assert.Contains(t, e.say(t, "не"), "не записвам")
}

func TestAMadeUpAmountIsNotKept(t *testing.T) {
	e := newBillEnv(t)
	f := madeUpFields
	f.Amount = "95,34"
	e.reads(t, f)
	out, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 x"))
	require.NoError(t, err)
	assert.Contains(t, out, "Не намерих в текста сумата")
	p, err := e.svc.Proposed(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, p)
}

func TestPaidMarksTheBillAndCancelsItsReminder(t *testing.T) {
	e := newBillEnv(t)
	f := madeUpFields
	f.Payment = bills.Transfer
	e.reads(t, f)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 x"))
	require.NoError(t, err)
	e.say(t, "да")
	active, _ := e.store.Active(context.Background(), e.userID)
	require.Len(t, active, 1)

	out := e.say(t, "платих телефона")
	assert.Contains(t, out, "Отбелязах като платена ✅")
	active, _ = e.store.Active(context.Background(), e.userID)
	assert.Empty(t, active, "the reminder of the due day is cancelled")
	out = e.say(t, "платих телефона")
	assert.Contains(t, out, "Видях го.", "nothing is unpaid: the model answers")
}

func TestAnUnsupportedFileIsSaidSo(t *testing.T) {
	e := newBillEnv(t)
	out, _ := e.svc.Read(context.Background(), e.userID, e.session, "notes.docx", []byte("PK\x03\x04"))
	assert.Contains(t, out, "PDF или снимка")
}

func TestAPageWithoutDatesIsFlaggedAsTheSameBill(t *testing.T) {
	e := newBillEnv(t)
	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "page1.pdf", []byte("%PDF-1.4 page one"))
	require.NoError(t, err)
	e.say(t, "да")
	f := madeUpFields
	f.InvoiceDate, f.DueDate = "", ""
	e.reads(t, f)
	out, err := e.svc.Read(context.Background(), e.userID, e.session, "page2.pdf", []byte("%PDF-1.4 page two"))
	require.NoError(t, err)
	assert.Contains(t, out, "Прилича на вече записана сметка")
}

func TestPaidSaysNothingOfAReminderItDidNotHave(t *testing.T) {
	e := newBillEnv(t)
	f := madeUpFields
	f.Payment, f.DueDate = bills.Transfer, ""
	e.reads(t, f)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 x"))
	require.NoError(t, err)
	e.say(t, "да")
	out := e.say(t, "платих телефона")
	assert.Contains(t, out, "Отбелязах като платена ✅")
	assert.NotContains(t, out, "Напомнянето")
}

// keepBill reads a made-up bill with these fields and keeps it.
func (e *billEnv) keepBill(t *testing.T, f bills.Fields, file string) {
	t.Helper()
	e.reads(t, f)
	text := "Beispiel GmbH\nRechnungsdatum " + f.InvoiceDate + "\nFällig am " + f.DueDate + "\nZu zahlender Betrag " + f.Amount + " €"
	svc := bills.NewService(e.db, fakeText{text}, fakeText{text}, bills.NewExtractor(e.model), e.store, nil)
	bills.SetClock(svc, func() time.Time { return reminderNow })
	_, err := svc.Read(context.Background(), e.userID, e.session, file, []byte("%PDF-1.4 "+file))
	require.NoError(t, err)
	require.Contains(t, e.say(t, "да"), "Записах сметката")
}

func TestTheAccountantAnswersFromTheKeptBills(t *testing.T) {
	e := newBillEnv(t)
	phoneNov := bills.Fields{IsBill: true, Issuer: "Beispiel Mobil", Kind: "phone_internet", Amount: "47,18", InvoiceDate: "12.11.2026", DueDate: "19.11.2026", Payment: bills.DirectDebit, Repeat: "monthly"}
	phoneOct := phoneNov
	phoneOct.Amount, phoneOct.InvoiceDate, phoneOct.DueDate = "52,40", "12.10.2026", "19.10.2026"
	power := bills.Fields{IsBill: true, Issuer: "Beispiel Strom", Kind: "electricity", Amount: "80,00", InvoiceDate: "20.11.2026", DueDate: "30.11.2026", Payment: bills.Transfer, Repeat: "monthly"}
	e.keepBill(t, phoneNov, "a.pdf")
	e.keepBill(t, phoneOct, "b.pdf")
	e.keepBill(t, power, "c.pdf")

	out := e.say(t, "колко са ми сметките този месец?")
	assert.Contains(t, out, "Сметките за ноември 2026: общо 127,18 € (2 сметки)")
	assert.Contains(t, out, "• ток — 80,00 € (1 сметка)")
	assert.Contains(t, out, "💡 Най-голямото перо е ток — 80,00 € (62 % от всичко).")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")

	out = e.say(t, "колко платих за телефон тази година")
	assert.Contains(t, out, "Сметките за телефон и интернет за 2026 г.: общо 99,58 € (2 сметки)")

	out = e.say(t, "сравни сметките за ноември с октомври")
	assert.Contains(t, out, "ноември 2026 — 127,18 € (2 сметки); октомври 2026 — 52,40 € (1 сметка)")

	out = e.say(t, "кои сметки не съм платил?")
	assert.Contains(t, out, "Неплатени: 1 сметка, общо 80,00 €")
	assert.Contains(t, out, "Beispiel Strom")

	// "платих тока" is no question: it marks the bill paid.
	assert.Contains(t, e.say(t, "платих тока"), "Отбелязах като платена ✅")
	assert.Contains(t, e.say(t, "кои сметки не съм платил?"), "Нямаш неплатени сметки")
}

func TestTheAccountantWithoutBills(t *testing.T) {
	e := newBillEnv(t)
	assert.Contains(t, e.say(t, "колко са ми сметките този месец?"), "Още нямаш записани сметки")
}

func TestTheMonthlyReportGoesOnceAMonth(t *testing.T) {
	e := newBillEnv(t)
	phone := bills.Fields{IsBill: true, Issuer: "Beispiel Mobil", Kind: "phone_internet", Amount: "47,18", InvoiceDate: "12.11.2026", DueDate: "19.11.2026", Payment: bills.DirectDebit, Repeat: "monthly"}
	e.keepBill(t, phone, "a.pdf")
	sent := 0
	notify := func(context.Context, uuid.UUID, string, string) { sent++ }

	// Before 09:00 on the 1st: nothing.
	bills.SetClock(e.svc, func() time.Time { return time.Date(2026, 12, 1, 8, 30, 0, 0, reminders.Location()) })
	require.NoError(t, e.svc.ReportPass(context.Background(), notify))
	bills.SetClock(e.svc, func() time.Time { return time.Date(2026, 12, 1, 9, 5, 0, 0, reminders.Location()) })
	require.NoError(t, e.svc.ReportPass(context.Background(), notify))
	require.NoError(t, e.svc.ReportPass(context.Background(), notify))
	msgs, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: e.session, Limit: 50})
	require.NoError(t, err)
	reports := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "📊 Сметките за ноември 2026: общо 47,18 €") {
			reports++
		}
	}
	assert.Equal(t, 1, reports, "one report, in the conversation of the bills")
	assert.Equal(t, 0, sent, "no Telegram chat is linked to the test user")
}

// readsStanding makes the model name the issuer and the kind of a standing bill.
func (e *billEnv) readsStanding(t *testing.T, f bills.StandingFields) {
	t.Helper()
	raw, err := json.Marshal(f)
	require.NoError(t, err)
	e.model.Response = openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: string(raw)}}}}
}

func (e *billEnv) at(day, hour int, month time.Month) {
	bills.SetClock(e.svc, func() time.Time { return time.Date(2026, month, day, hour, 0, 0, 0, reminders.Location()) })
}

// 2026-11-25, 16:09: "запише че на всяка 28 дата имам сметка от 14 евро към beispeil spiele club" was
// written by the model into a text file of its own. Now the code keeps it in the database.
func TestAStandingBillIsKeptAndItsPaymentsAreWrittenBeforeTheirDays(t *testing.T) {
	e := newBillEnv(t)
	e.readsStanding(t, bills.StandingFields{Issuer: "Beispiel Spiele Club", Kind: "subscription", Summary: "Абонамент за игри"})
	ctx := context.Background()

	out := e.say(t, "запише че на всяка 28 дата имам сметка от 14 евро към beispeil spiele club")
	assert.Contains(t, out, "Постоянна сметка:")
	assert.Contains(t, out, "Beispiel Spiele Club (абонаменти)")
	assert.Contains(t, out, "14,00 € всеки месец на 28-о число")
	assert.Contains(t, out, "Първото плащане: събота, 28.11.2026")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")

	out = e.say(t, "да")
	assert.Contains(t, out, "Записах постоянната сметка ✅")
	kept, err := e.svc.KeptStanding(ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, kept, 1)

	// The 24th: the 28th is more than three days away, nothing is written yet.
	assert.Contains(t, e.say(t, "колко са ми сметките този месец?"), "Още нямаш записани плащания")
	// The 25th: the payment of the 28th, with the reminder of the 27th; once.
	e.at(25, 12, 11)
	require.NoError(t, e.svc.StandingPass(ctx))
	require.NoError(t, e.svc.StandingPass(ctx))
	out = e.say(t, "колко са ми сметките този месец?")
	assert.Contains(t, out, "Сметките за ноември 2026: общо 14,00 € (1 сметка)")
	active, err := e.store.Active(ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, time.Date(2026, 11, 27, 9, 0, 0, 0, reminders.Location()), active[0].Next.In(reminders.Location()))
	assert.Contains(t, active[0].Text, "14,00 €")

	out = e.say(t, "какви сметки имаш записани")
	assert.Contains(t, out, "Постоянни сметки:")
	assert.Contains(t, out, "1. Beispiel Spiele Club (абонаменти), 14,00 € всеки месец на 28-о число")

	out = e.say(t, "сметките имат ли копие във базата данни или само в bils.txt?")
	assert.Contains(t, out, "ги пази кодът в базата данни")
	assert.Contains(t, out, "Сега там има 1 сметка")
	assert.Contains(t, out, "Постоянни сметки:")
	assert.Contains(t, out, "bills.txt) са само бележки")
	assert.Empty(t, e.chat.Requests, "still no chat model")

	// "Махни напомнянето за абонамента" is the reminders'; the standing bill goes with its own words.
	e.say(t, "махни напомнянето за абонамента")
	kept, _ = e.svc.KeptStanding(ctx, e.userID)
	require.Len(t, kept, 1, "a reminder was asked for, not the standing bill")
	out = e.say(t, "махни постоянната сметка за spiele club")
	assert.Contains(t, out, "Махнах постоянната сметка ✅ Beispiel Spiele Club")
	assert.Contains(t, out, "Предстоящото плащане и напомнянето му са махнати")
	kept, _ = e.svc.KeptStanding(ctx, e.userID)
	assert.Empty(t, kept)
	active, _ = e.store.Active(ctx, e.userID)
	assert.Empty(t, active, "the reminder of the payment to come is cancelled")
	assert.Contains(t, e.say(t, "колко са ми сметките този месец?"), "Още нямаш записани сметки", "the payment to come is gone")
	require.NoError(t, e.svc.StandingPass(ctx))
	assert.Contains(t, e.say(t, "колко са ми сметките този месец?"), "Още нямаш записани сметки", "and not written again")
}

func TestAStandingBillWithoutItsAmountIsNotProposed(t *testing.T) {
	e := newBillEnv(t)
	out := e.say(t, "запиши, че всеки месец плащам абонамент за игри")
	assert.Contains(t, out, "трябват сумата и деня от месеца")
	assert.Empty(t, e.model.Requests, "the model is not asked to read an incomplete request")
	p, err := e.svc.ProposedStanding(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, p)
	assert.Contains(t, e.say(t, "не"), "Видях го.", "no proposal waits: the model answers")
}

// A bill from a document of the same kind and issuer is the bill of its month: the payment of the
// standing bill is taken out, and not written again.
func TestADocumentTakesThePlaceOfTheStandingPaymentOfItsMonth(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	e.readsStanding(t, bills.StandingFields{Issuer: "Beispiel Telefon", Kind: "phone_internet", Summary: "Телефон"})
	require.Contains(t, e.say(t, "запиши, че всеки месец на 5-то плащам 50 € на Beispiel Telefon, изтегля се автоматично"), "изтегля се автоматично")
	require.Contains(t, e.say(t, "да"), "Записах постоянната сметка ✅")
	e.at(3, 10, 12)
	require.NoError(t, e.svc.StandingPass(ctx))
	require.Contains(t, e.say(t, "колко са сметките през декември 2026?"), "общо 50,00 € (1 сметка)")

	// A standing bill that waits is dropped by a document that is read: the "да" is for the document.
	e.readsStanding(t, bills.StandingFields{Issuer: "Beispiel Strom", Kind: "electricity"})
	require.Contains(t, e.say(t, "запиши, че всеки месец на 20-ти плащам 80 € за ток на Beispiel Strom"), "Постоянна сметка:")
	e.reads(t, madeUpFields) // Beispiel Telefon, 47,18 €, due 05.12.2026
	_, err := e.svc.Read(ctx, e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 december"))
	require.NoError(t, err)
	out := e.say(t, "да")
	assert.Contains(t, out, "Записах сметката ✅")
	assert.Contains(t, out, "заменя плащането от постоянната сметка")
	waiting, err := e.svc.ProposedStanding(ctx, e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, waiting, "the standing bill that waited was dropped")
	require.NoError(t, e.svc.StandingPass(ctx))
	assert.Contains(t, e.say(t, "колко са сметките през декември 2026?"), "общо 47,18 € (1 сметка)")
	active, _ := e.store.Active(ctx, e.userID)
	require.Len(t, active, 1, "the reminder of the standing payment went with it")
	assert.Contains(t, active[0].Text, "47,18 €")
}

// 2026-11-25: the first bill of October was kept on the 25th of November and the report of October
// went out at once. A report the 1st missed goes out up to the 7th, not later.
func TestALateReportGoesOnlyInTheFirstDaysOfTheMonth(t *testing.T) {
	e := newBillEnv(t)
	phone := bills.Fields{IsBill: true, Issuer: "Beispiel Mobil", Kind: "phone_internet", Amount: "47,18", InvoiceDate: "12.11.2026", DueDate: "19.11.2026", Payment: bills.DirectDebit, Repeat: "monthly"}
	e.keepBill(t, phone, "a.pdf")
	reports := func() int {
		msgs, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: e.session, Limit: 50})
		require.NoError(t, err)
		n := 0
		for _, m := range msgs {
			if strings.HasPrefix(m.Content, "📊 Сметките за ноември 2026") {
				n++
			}
		}
		return n
	}

	bills.SetClock(e.svc, func() time.Time { return time.Date(2026, 12, 8, 9, 5, 0, 0, reminders.Location()) })
	require.NoError(t, e.svc.ReportPass(context.Background(), nil))
	assert.Equal(t, 0, reports(), "the 8th is too late")

	bills.SetClock(e.svc, func() time.Time { return time.Date(2026, 12, 7, 23, 50, 0, 0, reminders.Location()) })
	require.NoError(t, e.svc.ReportPass(context.Background(), nil))
	assert.Equal(t, 1, reports(), "the 7th is still in time")
}
