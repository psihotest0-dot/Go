package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/llm"
)

// planStream runs a planned turn through the HTTP stream and returns the body.
func planStream(t *testing.T, session string, planEvents bool, chunks ...string) string {
	t.Helper()
	var stream []openai.ChatCompletionStreamResponse
	for _, c := range chunks {
		stream = append(stream, textChunk(c))
	}
	e, o, _ := plannedEnv(t, &llm.MockLLM{StreamChunks: stream})
	body, _ := json.Marshal(ChatRequest{SessionID: session, Content: tiredMessage, PlanEvents: planEvents})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

// The client that asks for it gets the memories the answer used, after the text and before
// [DONE] (step 2.5).
func TestAPlannedStreamSendsTheMemoriesItUsed(t *testing.T) {
	body := planStream(t, "pf-1", true, "Почини си на дивана [1].")
	frame := "event: plan\ndata: {\"remembered\":[\"" + sofaMemory + "\"]}\n\n"
	require.Contains(t, body, frame)
	assert.Less(t, strings.Index(body, "Почини си"), strings.Index(body, frame), "after the text")
	assert.Less(t, strings.Index(body, frame), strings.Index(body, "data: [DONE]"), "before the end")
}

// (One stream per test: the test environment changes the working directory.)
func TestNoPlanFrameForAClientThatDidNotAsk(t *testing.T) {
	assert.NotContains(t, planStream(t, "pf-2", false, "Почини си на дивана [1]."), "event: plan")
}

func TestNoPlanFrameWhenTheAnswerMarkedNoMemory(t *testing.T) {
	assert.NotContains(t, planStream(t, "pf-3", true, "Почини си на дивана."), "event: plan")
}

// Telegram gets the memories with the turn.
func TestATelegramTurnOfAPlanTellsTheMemoriesItUsed(t *testing.T) {
	e, o, _ := plannedEnv(t, &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Почини си [1].")}})
	turn, err := o.ProcessChatTurn(context.Background(), e.userID, ChatRequest{SessionID: "pf-tg", Content: tiredMessage})
	require.NoError(t, err)
	assert.Equal(t, "Почини си.", turn.Answer)
	assert.Equal(t, []string{sofaMemory}, turn.Remembered)
}
