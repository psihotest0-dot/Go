package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/bills"
	"go-monolith/internal/db"
	"go-monolith/internal/reminders"
	"go-monolith/internal/taskrun"
)

// regenerate presses "Регенерирай" on the last answer of the conversation.
func (e *billEnv) regenerate(t *testing.T) string {
	t.Helper()
	body, _ := json.Marshal(RegenerateRequest{SessionID: e.session})
	req := httptest.NewRequest(http.MethodPost, "/chat/regenerate", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	e.o.HandleRegenerate(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

// The code review of 2026-11-25 (not checked then): "Регенерирай" on an answer of the code ran the
// message through the code again. "платих 1" marked the first unpaid bill, and its regeneration
// marked the next one, which had become the first.
func TestRegenerateDoesNotRunTheCodeAgain(t *testing.T) {
	e := newBillEnv(t)
	insurance := bills.Fields{IsBill: true, Issuer: "Muster Versicherung", Kind: "insurance", Amount: "120,00", InvoiceDate: "10.11.2026", DueDate: "25.11.2026", Payment: bills.Transfer, Repeat: "yearly"}
	power := bills.Fields{IsBill: true, Issuer: "Beispiel Strom", Kind: "electricity", Amount: "80,00", InvoiceDate: "20.11.2026", DueDate: "30.11.2026", Payment: bills.Transfer, Repeat: "monthly"}
	e.keepBill(t, insurance, "a.pdf")
	e.keepBill(t, power, "b.pdf")

	require.Contains(t, e.say(t, "платих 1"), "Отбелязах като платена ✅ Muster Versicherung")
	out := e.regenerate(t)
	assert.Contains(t, out, "Отбелязах като платена ✅ Muster Versicherung", "the answer is said again as it was")
	assert.NotContains(t, out, "Beispiel Strom")

	assert.Contains(t, e.say(t, "кои сметки не съм платил?"), "1. Beispiel Strom", "the electricity is still unpaid")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
}

// "Регенерирай" on "Записах сметката ✅": the proposal is gone, and the "да" went to the model,
// which read a question with no answer after it.
func TestRegenerateOfAKeptBillDoesNotAskTheModel(t *testing.T) {
	e := newBillEnv(t)
	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	require.Contains(t, e.say(t, "да"), "Записах сметката ✅")

	out := e.regenerate(t)
	assert.Contains(t, out, "Записах сметката ✅")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
	msgs, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: e.session, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Contains(t, msgs[0].Content, "Записах сметката ✅", "the answer is kept")
	assert.Equal(t, "assistant", msgs[0].Role)
	assert.Equal(t, "user", msgs[1].Role, "one answer, not two")
}

// An answer of the model is still made anew.
func TestRegenerateOfAnAnswerOfTheModelAsksTheModel(t *testing.T) {
	e := newBillEnv(t)
	e.say(t, "здравей")
	require.Len(t, e.chat.Requests, 1)
	e.regenerate(t)
	assert.Len(t, e.chat.Requests, 2)
}

// writeServerNotice writes into the conversation what the server writes there on its own: a
// reminder (reminders.Runner.Pass) with its record.
func (e *billEnv) writeServerNotice(t *testing.T, text, log string) {
	t.Helper()
	_, err := e.db.Queries.CreateMessage(context.Background(), db.CreateMessageParams{
		UserID: e.userID, SessionID: e.session, Role: "assistant", Content: text,
		SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true},
		ToolLog:        sql.NullString{String: log, Valid: true},
	})
	require.NoError(t, err)
}

// The code review of 2026-11-25 (seen, not done): a bill waits for its yes, the server writes a
// reminder into the same conversation, and the "добре" to the reminder kept the bill.
func TestAYesToAReminderTheServerSentDoesNotKeepTheBillBeforeIt(t *testing.T) {
	e := newBillEnv(t)
	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	e.writeServerNotice(t, "⏰ Напомняне: да платя наема", reminders.SentLog+" 7")

	out := e.say(t, "добре")
	assert.NotContains(t, out, "Записах")
	assert.Contains(t, out, "още чака")
	p, err := e.svc.Proposed(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	require.NotNil(t, p, "the bill still waits")

	assert.Contains(t, e.say(t, "да"), "Записах сметката ✅", "the yes after the question again keeps it")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
}

// The same after the monthly report of the bills and after the report of a task.
func TestAYesToAReportOfTheServerDoesNotKeepTheBillBeforeIt(t *testing.T) {
	for _, log := range []string{bills.ReportLog + "октомври 2026", taskrun.ReportLog + "2 files written"} {
		t.Run(log, func(t *testing.T) {
			e := newBillEnv(t)
			e.reads(t, madeUpFields)
			_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
			require.NoError(t, err)
			e.writeServerNotice(t, "Справка", log)
			assert.NotContains(t, e.say(t, "да"), "Записах")
		})
	}
}

// Without a server notice in between, the yes keeps the bill as before.
func TestAYesRightAfterTheBillKeepsIt(t *testing.T) {
	e := newBillEnv(t)
	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	assert.Contains(t, e.say(t, "добре"), "Записах сметката ✅")
}

// The record of an answer of the code is not shown to the model: the prompt stays what it was.
func TestTheModelDoesNotSeeTheRecordOfAnAnswerOfTheCode(t *testing.T) {
	e := newBillEnv(t)
	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	require.Contains(t, e.say(t, "да"), "Записах сметката ✅")
	msgs, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: e.session, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, codeAnswerLog, msgs[0].ToolLog.String, "the answer is marked as the code's")

	e.say(t, "здравей")
	require.Len(t, e.chat.Requests, 1)
	for _, m := range e.chat.Requests[0].Messages {
		assert.NotContains(t, m.Content, codeAnswerLog)
		if strings.HasPrefix(m.Content, "Записах сметката") {
			assert.NotContains(t, m.Content, toolLogNotePrefix, "no record under the answer of the code")
		}
	}
}

// "Добре" after a reminder with nothing waiting is the model's, as before.
func TestAYesToAReminderWithNothingWaitingGoesToTheModel(t *testing.T) {
	e := newBillEnv(t)
	e.writeServerNotice(t, "⏰ Напомняне: да платя наема", reminders.SentLog+" 7")
	out := e.say(t, "добре")
	assert.NotContains(t, out, "още чака")
	assert.Len(t, e.chat.Requests, 1, "the model answers")
}
