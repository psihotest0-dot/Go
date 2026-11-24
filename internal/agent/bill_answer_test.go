package agent

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

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
