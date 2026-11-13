package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
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

// slowLLM answers like its inner model, but only after delay: a long generation.
type slowLLM struct {
	llm.LLMClient
	delay time.Duration
}

func (s slowLLM) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (llm.Stream, error) {
	time.Sleep(s.delay)
	return s.LLMClient.ChatStream(ctx, req)
}

// The context of the write starts when the answer is complete. One made before
// the generation expired during a long answer (over 30 s with a real model) and
// the answer was never stored ("context deadline exceeded").
func TestAnAnswerThatTakesLongerThanTheWriteTimeoutIsStillStored(t *testing.T) {
	old := persistTimeout
	persistTimeout = 50 * time.Millisecond
	t.Cleanup(func() { persistTimeout = old })

	f := newSanitizeFixture(t, "Дълъг отговор")
	f.orchestrator.llmClient = slowLLM{LLMClient: f.orchestrator.llmClient, delay: 150 * time.Millisecond}

	rec, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-slow", Content: "Въпрос"})
	f.orchestrator.HandleStreamChat(rec, req)
	require.Equal(t, []string{"Дълъг отговор"}, f.storedContents(t, "s-slow", "assistant"))

	f.seedTurn(t, "s-slow-regen", "Въпрос", "Стар отговор")
	rec, req = f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-slow-regen"})
	f.orchestrator.HandleRegenerate(rec, req)
	require.Equal(t, []string{"Дълъг отговор"}, f.storedContents(t, "s-slow-regen", "assistant"))
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
