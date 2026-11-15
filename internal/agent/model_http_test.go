package agent

import (
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// The choice of the model through the real handlers: the body of the request, the prompt of the turn,
// the stream, the stored answer. (model_routing_test.go calls generateResponse directly.)

func (f *sanitizeFixture) withPaid(answer string, library bool) *llm.MockLLM {
	paid := say(answer)
	f.orchestrator.WithPaidModel(paid, "deepseek-flash", library)
	return paid
}

func (f *sanitizeFixture) local() *llm.MockLLM { return f.orchestrator.llmClient.(*llm.MockLLM) }

func streamed(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := rec.Body.String()
	require.Contains(t, body, "data: [DONE]", "the stream ended well: %s", body)
	require.NotContains(t, body, "event: error")
	return body
}

func TestTheButtonSendsTheTurnToTheCloudAndTheAnswerIsStored(t *testing.T) {
	f := newSanitizeFixture(t, "локален отговор")
	paid := f.withPaid("облачен отговор", true)
	rec, req := f.request(t, "/chat/stream", map[string]any{"session_id": "s-cloud", "content": generalQuestion, "use_paid": true})

	f.orchestrator.HandleStreamChat(rec, req)

	body := streamed(t, rec)
	assert.Contains(t, body, "облачен отговор")
	assert.Contains(t, body, "event: model\ndata: {\"provider\":\"deepseek\",\"model\":\"deepseek-flash\"}")
	assert.Equal(t, []string{"облачен отговор"}, f.storedContents(t, "s-cloud", "assistant"))
	assert.NotEmpty(t, paid.Requests)
	assert.Empty(t, f.local().Requests, "the local model was not asked for this turn")
}

func TestWithoutTheChoiceAGeneralQuestionStaysHomeAndTheStreamSaysSo(t *testing.T) {
	f := newSanitizeFixture(t, "локален отговор")
	paid := f.withPaid("облачен отговор", true)
	rec, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-home", Content: generalQuestion})

	f.orchestrator.HandleStreamChat(rec, req)

	body := streamed(t, rec)
	assert.Contains(t, body, "локален отговор")
	assert.Contains(t, body, "event: model\ndata: {\"provider\":\"local\"}")
	assert.Empty(t, paid.Requests, "nothing left the machine")
}

func TestAQuestionToTheLibraryGoesToTheCloudByItselfAndHomeOnRequest(t *testing.T) {
	t.Run("by itself", func(t *testing.T) {
		f := newSanitizeFixture(t, "локален отговор")
		paid := f.withPaid("облачен отговор", true)
		rec, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-lib", Content: libraryQuestion})

		f.orchestrator.HandleStreamChat(rec, req)

		assert.Contains(t, streamed(t, rec), `"provider":"deepseek"`)
		assert.NotEmpty(t, paid.Requests)
		assert.Empty(t, f.local().Requests)
	})
	t.Run("not when the button says home", func(t *testing.T) {
		f := newSanitizeFixture(t, "локален отговор")
		paid := f.withPaid("облачен отговор", true)
		rec, req := f.request(t, "/chat/stream", map[string]any{"session_id": "s-lib-home", "content": libraryQuestion, "use_paid": false})

		f.orchestrator.HandleStreamChat(rec, req)

		assert.Contains(t, streamed(t, rec), `"provider":"local"`)
		assert.Empty(t, paid.Requests)
	})
	t.Run("not when LIBRARY_LLM is local", func(t *testing.T) {
		f := newSanitizeFixture(t, "локален отговор")
		paid := f.withPaid("облачен отговор", false)
		rec, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-lib-off", Content: libraryQuestion})

		f.orchestrator.HandleStreamChat(rec, req)

		assert.Contains(t, streamed(t, rec), `"provider":"local"`)
		assert.Empty(t, paid.Requests)
	})
}

// The cloud is down: the turn is answered at home, stored once, and the stream is a good one.
func TestWhenTheCloudFailsTheStreamStillEndsWellAtHome(t *testing.T) {
	f := newSanitizeFixture(t, "локален отговор")
	f.orchestrator.WithPaidModel(&llm.MockLLM{Error: llm.ErrSpendCap}, "deepseek-flash", true)
	rec, req := f.request(t, "/chat/stream", map[string]any{"session_id": "s-down", "content": generalQuestion, "use_paid": true})

	f.orchestrator.HandleStreamChat(rec, req)

	body := streamed(t, rec)
	assert.Contains(t, body, "локален отговор")
	assert.Contains(t, body, `"provider":"local"`, "the client is not told the cloud answered")
	assert.Equal(t, []string{"локален отговор"}, f.storedContents(t, "s-down", "assistant"))
}

func TestRegeneratingInTheCloudReplacesTheStoredAnswer(t *testing.T) {
	f := newSanitizeFixture(t, "локален отговор")
	paid := f.withPaid("нов облачен отговор", true)
	f.seedTurn(t, "s-regen-cloud", "Въпрос", "Стар отговор")
	rec, req := f.request(t, "/chat/regenerate", map[string]any{"session_id": "s-regen-cloud", "use_paid": true})

	f.orchestrator.HandleRegenerate(rec, req)

	body := streamed(t, rec)
	assert.Contains(t, body, `"provider":"deepseek"`)
	assert.Equal(t, []string{"нов облачен отговор"}, f.storedContents(t, "s-regen-cloud", "assistant"))
	assert.NotEmpty(t, paid.Requests)
}

// The prompt of a turn in the cloud is the one of every turn: the choice changes who is asked, not what.
func TestTheCloudGetsTheSamePromptAsTheLocalModel(t *testing.T) {
	f := newSanitizeFixture(t, "локален отговор")
	paid := f.withPaid("облачен отговор", true)
	rec, req := f.request(t, "/chat/stream", map[string]any{"session_id": "s-same", "content": generalQuestion, "use_paid": true})
	f.orchestrator.HandleStreamChat(rec, req)
	streamed(t, rec)
	cloudPrompt := paid.Requests[0].Messages

	rec, req = f.request(t, "/chat/stream", map[string]any{"session_id": "s-same-2", "content": generalQuestion, "use_paid": false})
	f.orchestrator.HandleStreamChat(rec, req)
	streamed(t, rec)
	homePrompt := f.local().Requests[0].Messages

	require.NotEmpty(t, cloudPrompt)
	assert.Equal(t, openai.ChatMessageRoleSystem, cloudPrompt[0].Role)
	assert.Equal(t, len(homePrompt), len(cloudPrompt))
	assert.Equal(t, strings.Contains(homePrompt[len(homePrompt)-1].Content, generalQuestion), strings.Contains(cloudPrompt[len(cloudPrompt)-1].Content, generalQuestion))
}

func TestWithoutAPaidModelTheHandlerNeverMentionsTheModel(t *testing.T) {
	f := newSanitizeFixture(t, "локален отговор")
	rec, req := f.request(t, "/chat/stream", map[string]any{"session_id": "s-none", "content": generalQuestion, "use_paid": true})

	f.orchestrator.HandleStreamChat(rec, req)

	body := streamed(t, rec)
	assert.NotContains(t, body, "event: model", "the choice does nothing, and the stream is what it always was")
	assert.Contains(t, body, "локален отговор")
}
