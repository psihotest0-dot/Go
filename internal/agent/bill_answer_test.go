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

// The code review of 2026-11-27: the list of all the kept bills of a kind was made from the newest
// 200 bills of every kind, so with more of them its sum was wrong and said nothing of it.
func TestTheListOfAllTheBillsHasThemAll(t *testing.T) {
	e := newBillEnv(t)
	_, err := e.db.SqlDB.Exec(`INSERT INTO bills (user_id, session_id, issuer, kind, amount_cents, due_date, payment, repeat, file_name, file_hash, status)
		SELECT $1, 'bills', 'Beispiel Strom', 'electricity', 100, DATE '2020-01-01' + n, 'direct_debit', 'monthly', 'x.pdf', 'power-' || n, 'kept'
		FROM generate_series(1, 30) AS n`, e.userID)
	require.NoError(t, err)
	_, err = e.db.SqlDB.Exec(`INSERT INTO bills (user_id, session_id, issuer, kind, amount_cents, due_date, payment, repeat, file_name, file_hash, status)
		SELECT $1, 'bills', 'Beispiel Mobil', 'phone_internet', 100, DATE '2024-01-01' + n, 'direct_debit', 'monthly', 'x.pdf', 'phone-' || n, 'kept'
		FROM generate_series(1, 200) AS n`, e.userID)
	require.NoError(t, err)

	out := e.say(t, "какви сметки за ток имам записани")
	assert.Contains(t, out, "Всички записани сметки за ток (30 сметки, общо 30,00 €)", "the old ones, behind 200 newer ones, are there")
	assert.Contains(t, e.say(t, "какви сметки имам записани"), "Всички записани сметки (230 сметки, общо 230,00 €)")
}

// The code review of 2026-11-27: the sum of two months that do not follow one another.
func TestTheSumOfTwoMonthsHasBothAndNotTheOneBetween(t *testing.T) {
	e := newBillEnv(t)
	jan := bills.Fields{IsBill: true, Issuer: "Beispiel Mobil", Kind: "phone_internet", Amount: "10,00", InvoiceDate: "05.01.2026", DueDate: "15.01.2026", Payment: bills.DirectDebit, Repeat: "monthly"}
	feb, mar := jan, jan
	feb.Amount, feb.InvoiceDate, feb.DueDate = "20,00", "05.02.2026", "15.02.2026"
	mar.Amount, mar.InvoiceDate, mar.DueDate = "40,00", "05.03.2026", "15.03.2026"
	e.keepBill(t, jan, "jan.pdf")
	e.keepBill(t, feb, "feb.pdf")
	e.keepBill(t, mar, "mar.pdf")

	assert.Contains(t, e.say(t, "колко платих за януари и март"), "Сметките за януари 2026 и март 2026: общо 50,00 € (2 сметки)")
	assert.Contains(t, e.say(t, "колко платих за януари и февруари"), "Сметките за януари – февруари 2026: общо 30,00 € (2 сметки)")
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

// 2026-11-24 21:12 and 2026-11-25 16:57: a photo that is no bill got "не прилича на сметка" and
// nothing else, and "можеш ли да прочетеш какво пише?" had nothing to read. Now its text is attached.
func TestTheTextOfAnUploadThatIsNoBillIsAttached(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	e.svc.WithAttachments(func(ctx context.Context, userID uuid.UUID, session, name, text string) error {
		_, err := e.reminderEnv.attachmentEnv.svc.Add(ctx, userID, session, name, text)
		return err
	})
	e.reads(t, bills.Fields{IsBill: false})
	out, err := e.svc.Read(ctx, e.userID, e.session, "snimka.jpg", []byte("%PDF-1.4 page"))
	require.NoError(t, err)
	assert.Contains(t, out, "Това не прилича на сметка")
	assert.Contains(t, out, "прикачих към разговора като „snimka.jpg.txt“")
	f, err := e.reminderEnv.attachmentEnv.svc.Find(ctx, e.userID, e.session, "snimka.jpg.txt")
	require.NoError(t, err)
	assert.Contains(t, f.Content, "Beispiel Telefon GmbH", "the text the code read")
	assert.Contains(t, f.Content, "може да има грешно разчетени букви")

	// A bill without its amount: the same.
	f2 := madeUpFields
	f2.Amount = "95,34"
	e.reads(t, f2)
	out, _ = e.svc.Read(ctx, e.userID, e.session, "kopie.pdf", []byte("%PDF-1.4 other"))
	assert.Contains(t, out, "Не намерих в текста сумата")
	assert.Contains(t, out, "„kopie.pdf.txt“")

	// Without the attachments nothing is promised.
	plain := bills.NewService(e.db, fakeText{madeUpBill}, fakeText{madeUpBill}, bills.NewExtractor(e.model), e.store, nil)
	e.reads(t, bills.Fields{IsBill: false})
	out, _ = plain.Read(ctx, e.userID, e.session, "a.jpg", []byte("%PDF-1.4 third"))
	assert.NotContains(t, out, "прикачих")
}

// 2026-11-25, 18:04-18:05 (Telegram): "раздели ги на годишно плащане колко дължа и колко за
// месечните" got "Нямаш неплатени сметки", and "изчисли … годишните … месечните" the sum of this year,
// without the standing bills and next year's.
func TestTheMonthlyAndTheYearlyPaymentsSideBySide(t *testing.T) {
	e := newBillEnv(t)
	phone := bills.Fields{IsBill: true, Issuer: "Beispiel Mobil", Kind: "phone_internet", Amount: "40,00", InvoiceDate: "09.10.2026", DueDate: "16.10.2026", Payment: bills.DirectDebit, Repeat: "monthly"}
	phoneOld := phone
	phoneOld.Amount, phoneOld.InvoiceDate, phoneOld.DueDate = "38,00", "09.09.2026", "16.09.2026"
	insurance := bills.Fields{IsBill: true, Issuer: "Beispiel Versicherung", Kind: "insurance", Amount: "120,00", InvoiceDate: "02.11.2026", DueDate: "01.01.2027", Payment: bills.DirectDebit, Repeat: "yearly"}
	doctor := bills.Fields{IsBill: true, Issuer: "Praxis Beispiel", Kind: "doctor", Amount: "80,00", InvoiceDate: "12.11.2026", DueDate: "26.11.2026", Payment: bills.Transfer, Repeat: "once"}
	e.keepBill(t, phoneOld, "p1.pdf")
	e.keepBill(t, phone, "p2.pdf")
	e.keepBill(t, insurance, "i.pdf")
	e.keepBill(t, doctor, "d.pdf")
	e.readsStanding(t, bills.StandingFields{Kind: "rent", Summary: "Наем"})
	require.Contains(t, e.say(t, "запиши, че всяко първо число плащам 500 € наем, изтегля се автоматично"), "Постоянна сметка:")
	require.Contains(t, e.say(t, "да"), "Записах постоянната сметка ✅")

	for _, q := range []string{
		"Можеш ли да ги разделиш на годишно плащане колко дължа и колко за месечните",
		"Лили изчисли какво е плащането за годишните сметки и каква е сумата за месечните",
	} {
		out := e.say(t, q)
		assert.Contains(t, out, "Месечни плащания (на месец): 540,00 €", q)
		assert.Contains(t, out, "наем — 500,00 € (постоянна, всеки месец на 1-о число)", q)
		assert.Contains(t, out, "Beispiel Mobil (телефон и интернет) — 40,00 € (последната сметка: срок петък, 16.10.2026)", q)
		assert.NotContains(t, out, "38,00 €", "only the latest bill of an issuer")
		assert.Contains(t, out, "Годишни плащания (на година): 120,00 €", q)
		assert.Contains(t, out, "За една година: 6 600,00 € (540,00 € × 12 + 120,00 €).", q)
		assert.Contains(t, out, "Еднократните сметки (1) не са в тази сметка.", q)
	}
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
	assert.Contains(t, e.say(t, "колко са ми сметките този месец?"), "Сметките за ноември 2026", "a month is still a month")

	// The payment of the rent is written, then the rent is removed: neither counts any more.
	e.at(28, 12, 11)
	require.NoError(t, e.svc.StandingPass(context.Background()))
	e.at(2, 12, 12) // the payment of 01.12 has gone by: it stays when the rent is removed
	require.Contains(t, e.say(t, "махни постоянната сметка за наем"), "Махнах постоянната сметка ✅")
	out := e.say(t, "колко са месечните и годишните плащания?")
	assert.Contains(t, out, "Месечни плащания (на месец): 40,00 €")
	assert.NotContains(t, out, "наем")
}

// 2026-11-25, 18:33-18:35 (Telegram): the last day of a lease was not kept, and "до 01.06.2028"
// became "денят 1"; "Бе" dropped the bill that waited.
func TestAStandingBillHasALastDay(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	e.readsStanding(t, bills.StandingFields{Kind: "car", Summary: "Лизинг за автомобил"})
	out := e.say(t, "Запиши че всеки месец на 1во число до 01.05.2028 включително ,плащам 240,00 евро лизинг за автомобил,автоматично ,без напомняне")
	assert.Contains(t, out, "240,00 € всеки месец на 1-о число до 01.05.2028 включително — изтегля се автоматично")
	assert.Contains(t, out, "без напомняне")

	assert.Contains(t, e.say(t, "Бе"), "още чака")
	out = e.say(t, "Можеш ли да добавиш че е плащането е до 01.06.2028")
	assert.Contains(t, out, "Поправих по твоите думи: до четвъртък, 01.06.2028 включително")
	assert.Contains(t, out, "всеки месец на 1-о число до 01.06.2028 включително")
	assert.NotContains(t, out, "денят")
	require.Contains(t, e.say(t, "да"), "Записах постоянната сметка ✅")
	kept, err := e.svc.KeptStanding(ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, kept, 1)
	require.NotNil(t, kept[0].Until)
	assert.Equal(t, reminders.Day{Year: 2028, Month: 6, Day: 1}, *kept[0].Until)
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")

	// After its last day it is no monthly payment any more.
	assert.Contains(t, e.say(t, "колко са месечните и годишните плащания?"), "240,00 €")
	bills.SetClock(e.svc, func() time.Time { return time.Date(2028, 7, 1, 12, 0, 0, 0, reminders.Location()) })
	assert.Contains(t, e.say(t, "колко са месечните и годишните плащания?"), "Нямам записани месечни или годишни плащания")
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

// 2026-11-25, 16:52-16:55: a bill read without a due day was answered "Има срок - следващата дата е
// 15.09.2027", "промени я, че следващото плащане е 87,00 евро", "една седмица по рано". Each dropped
// the proposal and the model said it had done it. Now the code changes the proposal.
func TestTheAnswerToAProposalChangesIt(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	f := madeUpFields
	f.DueDate, f.Repeat = "", "yearly"
	e.reads(t, f)
	_, err := e.svc.Read(ctx, e.userID, e.session, "snimka.jpg", []byte("%PDF-1.4 club"))
	require.NoError(t, err)

	out := e.say(t, "Има срок -следващата дата е 15.09.2027")
	assert.Contains(t, out, "Поправих по твоите думи: срок сряда, 15.09.2027")
	assert.Contains(t, out, "Още не е записана")
	assert.Contains(t, out, "Ще ти напомня във вторник, 14.09.2027, в 09:00")
	out = e.say(t, "Да но я промени че следващото плащане е 87,00 евро")
	assert.Contains(t, out, "сума 87,00 €")
	assert.Contains(t, out, "Сума: 87,00 €")
	out = e.say(t, "Една седмица по рано")
	assert.Contains(t, out, "напомням в 09:00 7 дни преди срока")
	assert.Contains(t, out, "Ще ти напомня в сряда, 08.09.2027, в 09:00")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")

	out = e.say(t, "да")
	assert.Contains(t, out, "Записах сметката ✅")
	assert.Contains(t, out, "87,00 €")
	active, err := e.store.Active(ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, time.Date(2027, 9, 8, 9, 0, 0, 0, reminders.Location()), active[0].Next.In(reminders.Location()))

	// The bill of next year is in the list of the kept bills (16:06 it was not).
	out = e.say(t, "какви сметки имам записани")
	assert.Contains(t, out, "Всички записани сметки (1 сметка, общо 87,00 €)")
	assert.Contains(t, out, "15.09.2027")
}

// "до" a day, said of a bill from a document, is its due day.
func TestUntilADayIsTheDueDayOfADocument(t *testing.T) {
	e := newBillEnv(t)
	f := madeUpFields
	f.DueDate = ""
	e.reads(t, f)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 until"))
	require.NoError(t, err)
	out := e.say(t, "платима е до 15.12.2026")
	assert.Contains(t, out, "Срок: вторник, 15.12.2026")
}

// A change and a yes in one answer: the bill is kept with the change.
func TestAChangeWithAYesKeepsTheBill(t *testing.T) {
	e := newBillEnv(t)
	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 yes"))
	require.NoError(t, err)
	out := e.say(t, "без напомняне, да")
	assert.Contains(t, out, "Поправих по твоите думи: без напомняне")
	assert.Contains(t, out, "Записах сметката ✅")
	assert.Contains(t, out, "Без напомняне: не искаш напомняне за нея")
	active, _ := e.store.Active(context.Background(), e.userID)
	assert.Empty(t, active)
}

// 17:04-17:06: "изтегля се автоматично, да" to a standing bill dropped it and the model said
// "Записах в базата"; "няма нужда от напомняне" became a text file.
func TestAStandingBillIsChangedAndKeptByTheAnswer(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	e.readsStanding(t, bills.StandingFields{Issuer: "", Kind: "rent", Summary: "Наем за квартира"})
	require.Contains(t, e.say(t, "запиши че всеки месец на 1-во число плащам по 640 евро за квартира"), "Постоянна сметка:")

	out := e.say(t, "изтегля се автоматично,да")
	assert.Contains(t, out, "Поправих по твоите думи: изтегля се автоматично")
	assert.Contains(t, out, "Записах постоянната сметка ✅")
	kept, err := e.svc.KeptStanding(ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, kept, 1)
	assert.Equal(t, bills.DirectDebit, kept[0].Payment)

	// Without a reminder from the start.
	e.readsStanding(t, bills.StandingFields{Issuer: "Beispiel Strom", Kind: "electricity"})
	out = e.say(t, "запиши, че всеки месец на 26-ти плащам 80 € за ток на Beispiel Strom, без напомняне")
	assert.Contains(t, out, "без напомняне")
	require.Contains(t, e.say(t, "да"), "Записах постоянната сметка ✅")
	e.at(25, 12, 11)
	require.NoError(t, e.svc.StandingPass(ctx))
	assert.Contains(t, e.say(t, "колко са ми сметките този месец?"), "80,00 €", "the payment is written")
	active, _ := e.store.Active(ctx, e.userID)
	assert.Empty(t, active, "but it has no reminder")
}

// The model's "ще те напомня…" with no reminder kept gets a line under it.
func TestAClaimedReminderIsCorrected(t *testing.T) {
	const claim = "Добре, ще те напомня точно една седмица преди 15.09.2027 за плащането на 87,00 евро."
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(claim)}}}
	o := &Orchestrator{llmClient: mock, tools: &fakeToolRegistry{}}
	content, _, err := o.generateResponse(context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Една седмица по рано"}}, toolDefinitions(), nil)
	require.NoError(t, err)
	assert.Equal(t, claim+"\n\n"+billClaimCorrection, content)

	for answer, want := range map[string]bool{
		"Записах в базата, че наемът за квартира се изтегля автоматично.": true,
		"Разбрах, актуализирам сумата на 87,00 евро.":                     true,
		"Напомнянето ще включва сумата.":                                  true,
		"Искаш ли да ти напомня един ден по-рано?":                        false,
		"Ако искаш, ще ти напомня — кажи „напомни ми на …“.":              false,
		"Записах го в паметта си.":                                        false,
		"Добро утро!": false,
		// 2026-11-25, 17:37-17:40, no tool called in any of them.
		"Добавих наема от 640 евро, който се изтегля автоматично на всяко първо число.":  true,
		"Готово, промених статуса му и оттук нататък наемът ще се появява в списъка ти.": true,
	} {
		assert.Equal(t, want, claimsABillOrReminder(answer, false), answer)
	}
	// Through the turn: "Добавих наема…" with no tool called gets the line.
	const added = "Добавих наема от 640 евро към списъка ти с активните сметки."
	mock = &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(added)}}}
	o = &Orchestrator{llmClient: mock, tools: &fakeToolRegistry{}}
	content, _, err = o.generateResponse(context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "да добави я"}}, toolDefinitions(), nil)
	require.NoError(t, err)
	assert.Equal(t, added+"\n\n"+billClaimCorrection, content)

	// With a tool called, "добавих … в списъка" may be true of a file.
	assert.False(t, claimsABillOrReminder("Добавих реда в списъка във файла бележки.md.", true))
	assert.True(t, claimsABillOrReminder("Записах в базата сметката.", true), "the plain claims count either way")
}

// 2026-11-25, 17:54-17:55: the request lacked the amount and the next message was "14 евро"; the
// model answered it. Now the code puts the two together. "нямам нужда от напомняне" is no reminder.
func TestTheMissingAmountComesInTheNextMessage(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	e.readsStanding(t, bills.StandingFields{Issuer: "beispiel spiele club", Kind: "subscription", Summary: "Абонамент за игри"})
	out := e.say(t, "запиши че на всяко 28 число от месеца имам плащане за beispiel spiele club -автоматично е ,нямам нужда от напомняне")
	assert.Contains(t, out, "трябват сумата")
	out = e.say(t, "14 евро")
	assert.Contains(t, out, "Постоянна сметка:")
	assert.Contains(t, out, "14,00 € всеки месец на 28-о число — изтегля се автоматично")
	assert.Contains(t, out, "без напомняне")
	assert.Contains(t, e.say(t, "да"), "Записах постоянната сметка ✅")
	e.at(25, 12, 11)
	require.NoError(t, e.svc.StandingPass(ctx))
	active, _ := e.store.Active(ctx, e.userID)
	assert.Empty(t, active, "no reminder, as asked")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")

	// After another answer a bare amount is the model's again.
	e.say(t, "какви сметки имам")
	e.say(t, "14 евро")
	assert.NotEmpty(t, e.chat.Requests)
}

func TestOnlyTheLastAnswerCanAskForTheMissingPiece(t *testing.T) {
	msg := func(role, content string) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: role, Content: content}
	}
	asked := msg(openai.ChatMessageRoleAssistant, bills.StandingIncomplete+"сумата.")
	request := msg(openai.ChatMessageRoleUser, "запиши, че всеки месец на 5-то плащам наем")
	assert.Equal(t, "запиши, че всеки месец на 5-то плащам наем", incompleteStandingRequest([]openai.ChatCompletionMessage{request, asked}))
	assert.Empty(t, incompleteStandingRequest([]openai.ChatCompletionMessage{
		request, asked, msg(openai.ChatMessageRoleUser, "какво ново"), msg(openai.ChatMessageRoleAssistant, "Нищо особено."),
	}), "another answer came after it")
	assert.Empty(t, incompleteStandingRequest(nil))
}

// 2026-11-25, 17:35 and 17:37: four bills in one message became one; "всяко първо число" was not read.
func TestSeveralStandingBillsAreAskedOneByOneAndADayInAWordIsRead(t *testing.T) {
	e := newBillEnv(t)
	out := e.say(t, "добави липсващите -Наем за квартира: 640,00 € на 1-о число всеки месец (автоматично изтегляне)\n"+
		"Beispiel Telefon: 40,00 € на 10-то число всеки месец\nBeispiel Spiele: 14,00 € на 28-о число всеки месец")
	assert.Contains(t, out, "Виждам 3 сметки в едно съобщение")
	assert.Contains(t, out, "Нищо не е записано")
	p, err := e.svc.ProposedStanding(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, p)

	e.readsStanding(t, bills.StandingFields{Kind: "rent", Summary: "Наем"})
	out = e.say(t, "добави към сметките :Наем - 640 евро-всяко първо число на месеца- автоматично изтегляне")
	assert.Contains(t, out, "640,00 € всеки месец на 1-о число — изтегля се автоматично")
	assert.Contains(t, out, "Първото плащане: вторник, 01.12.2026")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
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

// The code review of 2026-11-25: "спри абонамента за Netflix" removed the only standing bill, which
// was another one. A standing bill is removed only when the message names it.
func TestAStandingBillIsRemovedOnlyWhenTheMessageNamesIt(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	e.readsStanding(t, bills.StandingFields{Issuer: "Beispiel Spiele Club", Kind: "subscription", Summary: "Абонамент за игри"})
	require.Contains(t, e.say(t, "запише че на всяка 28 дата имам сметка от 14 евро към beispeil spiele club"), "Постоянна сметка:")
	require.Contains(t, e.say(t, "да"), "Записах постоянната сметка ✅")

	out := e.say(t, "спри абонамента за Netflix")
	assert.Contains(t, out, "Не намерих такава постоянна сметка — нищо не е махнато")
	assert.Contains(t, out, "1. Beispiel Spiele Club")
	out = e.say(t, "махни абонамента")
	assert.Contains(t, out, "Коя постоянна сметка да махна?", "the words of the request name no bill")
	kept, err := e.svc.KeptStanding(ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, kept, 1, "nothing is removed before the person names it")

	assert.Contains(t, e.say(t, "махни постоянна 1"), "Махнах постоянната сметка ✅ Beispiel Spiele Club")
	kept, err = e.svc.KeptStanding(ctx, e.userID)
	require.NoError(t, err)
	assert.Empty(t, kept)
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
}

// The code review of 2026-11-25: the list of the unpaid bills of one kind was numbered from 1, and
// "платих 1" marked the first of ALL the unpaid ones; "платих тока на 2-ри" marked the second.
func TestPaidReadsTheNumberTheListGave(t *testing.T) {
	e := newBillEnv(t)
	insurance := bills.Fields{IsBill: true, Issuer: "Muster Versicherung", Kind: "insurance", Amount: "120,00", InvoiceDate: "10.11.2026", DueDate: "25.11.2026", Payment: bills.Transfer, Repeat: "yearly"}
	power := bills.Fields{IsBill: true, Issuer: "Beispiel Strom", Kind: "electricity", Amount: "80,00", InvoiceDate: "20.11.2026", DueDate: "30.11.2026", Payment: bills.Transfer, Repeat: "monthly"}
	e.keepBill(t, insurance, "a.pdf")
	e.keepBill(t, power, "b.pdf")

	out := e.say(t, "кои сметки за ток не съм платил?")
	assert.Contains(t, out, "2. Beispiel Strom", "the place among all the unpaid ones")
	assert.Contains(t, out, "„платих 2“")
	assert.NotContains(t, out, "1. Beispiel Strom")
	assert.Contains(t, e.say(t, "платих 2"), "Отбелязах като платена ✅ Beispiel Strom")

	out = e.say(t, "кои сметки не съм платил?")
	assert.Contains(t, out, "1. Muster Versicherung", "the insurance is still unpaid")
	assert.NotContains(t, out, "Beispiel Strom")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
}

func TestPaidWithADayInTheMessageMarksTheBillOfItsKind(t *testing.T) {
	e := newBillEnv(t)
	insurance := bills.Fields{IsBill: true, Issuer: "Muster Versicherung", Kind: "insurance", Amount: "120,00", InvoiceDate: "10.11.2026", DueDate: "25.11.2026", Payment: bills.Transfer, Repeat: "yearly"}
	power := bills.Fields{IsBill: true, Issuer: "Beispiel Strom", Kind: "electricity", Amount: "80,00", InvoiceDate: "20.11.2026", DueDate: "30.11.2026", Payment: bills.Transfer, Repeat: "monthly"}
	e.keepBill(t, insurance, "a.pdf")
	e.keepBill(t, power, "b.pdf")

	// "1" is the day of the payment, not the first bill of the list (the insurance).
	assert.Contains(t, e.say(t, "платих тока на 1-ви"), "Отбелязах като платена ✅ Beispiel Strom")
}

// A bill or a standing bill said back and never answered is forgotten after a week (the code
// review of 2026-11-25: the query was there, nothing called it).
func TestProposalsNobodyAnsweredAreForgottenAfterAWeek(t *testing.T) {
	e := newBillEnv(t)
	ctx := context.Background()
	e.reads(t, madeUpFields)
	_, err := e.svc.Read(ctx, e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	e.readsStanding(t, bills.StandingFields{Issuer: "Beispiel Spiele Club", Kind: "subscription", Summary: "Абонамент за игри"})
	_, handled := e.svc.ProposeStanding(ctx, e.userID, "other", "запише че на всяка 28 дата имам сметка от 14 евро към beispeil spiele club")
	require.True(t, handled)

	e.svc.DropStaleProposals(ctx, time.Now().Add(6*24*time.Hour))
	bill, err := e.svc.Proposed(ctx, e.userID, e.session)
	require.NoError(t, err)
	assert.NotNil(t, bill, "six days: still waiting")

	e.svc.DropStaleProposals(ctx, time.Now().Add(8*24*time.Hour))
	bill, err = e.svc.Proposed(ctx, e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, bill, "eight days: forgotten")
	standing, err := e.svc.ProposedStanding(ctx, e.userID, "other")
	require.NoError(t, err)
	assert.Nil(t, standing)
}
