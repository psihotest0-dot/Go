package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
)

// doneWatcher is a response recorder that runs onDone at the moment the final
// "[DONE]" frame is written, before the client could have read it.
type doneWatcher struct {
	*httptest.ResponseRecorder
	onDone func()
	seen   bool
}

func (d *doneWatcher) Write(p []byte) (int, error) {
	if !d.seen && bytes.Contains(p, []byte("[DONE]")) {
		d.seen = true
		d.onDone()
	}
	return d.ResponseRecorder.Write(p)
}

func (f *sanitizeFixture) seedTurn(t *testing.T, sessionID, question, answer string) {
	t.Helper()
	for _, m := range []struct{ role, content string }{{"user", question}, {"assistant", answer}} {
		_, err := f.database.Queries.CreateMessage(context.Background(), db.CreateMessageParams{
			UserID: f.userID, SessionID: sessionID, Role: m.role, Content: m.content,
		})
		require.NoError(t, err)
	}
}

// A client that reads the history (or asks the next question) as soon as the stream
// ends must find the answer: it is stored before [DONE] is sent, not after.
func TestStreamChatStoresTheAnswerBeforeDone(t *testing.T) {
	f := newSanitizeFixture(t, "Отговор ", "на въпроса")
	rec, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-done", Content: "Въпрос"})
	var atDone []string
	w := &doneWatcher{ResponseRecorder: rec, onDone: func() { atDone = f.storedContents(t, "s-done", "assistant") }}

	f.orchestrator.HandleStreamChat(w, req)

	require.True(t, w.seen, "the stream must end with [DONE]")
	require.Equal(t, []string{"Отговор на въпроса"}, atDone)
}

// The same for a regenerated answer: at [DONE] the new answer has replaced the
// old one; the history is never without an answer or with two.
func TestRegenerateReplacesTheAnswerBeforeDone(t *testing.T) {
	f := newSanitizeFixture(t, "Нов отговор")
	f.seedTurn(t, "s-regen-done", "Въпрос", "Стар отговор")
	rec, req := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-regen-done"})
	var atDone []string
	w := &doneWatcher{ResponseRecorder: rec, onDone: func() { atDone = f.storedContents(t, "s-regen-done", "assistant") }}

	f.orchestrator.HandleRegenerate(w, req)

	require.True(t, w.seen, "the stream must end with [DONE]")
	require.Equal(t, []string{"Нов отговор"}, atDone)
}

// The delete of the old answer and the insert of the new one are one transaction:
// when the insert fails (PostgreSQL refuses a NUL byte in text) the old answer is
// still there. As two separate writes both answers were lost.
func TestRegenerateKeepsTheOldAnswerWhenTheNewOneCannotBeStored(t *testing.T) {
	f := newSanitizeFixture(t, "Нов\x00отговор")
	f.seedTurn(t, "s-regen-tx", "Въпрос", "Стар отговор")
	rec, req := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-regen-tx"})

	f.orchestrator.HandleRegenerate(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"Стар отговор"}, f.storedContents(t, "s-regen-tx", "assistant"))
}

// openapi.yaml promises a list in the first "sources" frame, "[]" when the prompt
// has no sources; a nil slice used to go out as "null".
func TestStreamResponseSendsAnEmptyListOfSourcesNotNull(t *testing.T) {
	o := newGenerateOrchestrator(&llm.MockLLM{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", nil)
	rec := httptest.NewRecorder()

	_, _, err := o.streamResponse(rec, req, "s", nil, nil, nil, nil)

	require.NoError(t, err)
	first := strings.SplitN(rec.Body.String(), "\n\n", 2)[0]
	require.Equal(t, "event: sources\ndata: []", first)
}
