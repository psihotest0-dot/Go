package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

const (
	libraryQuestion = "Какво казва Марк Аврелий за смъртта?"
	generalQuestion = "Дай ми рецепта за палачинки."
)

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// routed builds an orchestrator with a local and a paid model, each with its own script.
func routed(local, paid *llm.MockLLM, library bool) *Orchestrator {
	o := &Orchestrator{llmClient: local, tools: &fakeToolRegistry{result: "ok"}}
	return o.WithPaidModel(paid, "deepseek-flash", library)
}

func say(text string) *llm.MockLLM {
	return &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(text)}}}
}

func ask(t *testing.T, o *Orchestrator, ctx context.Context, question string) (string, answeredBy, error) {
	t.Helper()
	ctx, _ = withTurn(ctx, "s", false)
	content, _, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: question}}, toolDefinitions(), nil)
	return content, turnFrom(ctx).answeredByWhom(), err
}

func routeCount(route, reason string) float64 {
	var c prometheus.Counter = metrics.ChatRoutes.WithLabelValues(route, reason)
	var m dto.Metric
	_ = c.Write(&m)
	return m.GetCounter().GetValue()
}

func TestWithoutAPaidModelNothingChanges(t *testing.T) {
	local := say("локално")
	o := &Orchestrator{llmClient: local, tools: &fakeToolRegistry{}}

	content, by, err := ask(t, o, context.Background(), libraryQuestion)

	require.NoError(t, err)
	assert.Equal(t, "локално", content)
	assert.Equal(t, answeredBy{}, by, "nothing is recorded, and so no frame is sent")
	assert.Empty(t, o.modelEvent(context.Background()))
}

func TestAQuestionToTheLibraryGoesToThePaidModel(t *testing.T) {
	local, paid := say("локално"), say("облак")
	before := routeCount("paid", "library")

	content, by, err := ask(t, routed(local, paid, true), context.Background(), libraryQuestion)

	require.NoError(t, err)
	assert.Equal(t, "облак", content)
	assert.Equal(t, answeredBy{Provider: "deepseek", Model: "deepseek-flash"}, by)
	assert.Empty(t, local.Requests, "the local model was not asked")
	assert.NotEmpty(t, paid.Requests)
	assert.EqualValues(t, 1, routeCount("paid", "library")-before)
}

func TestAnythingElseStaysLocal(t *testing.T) {
	local, paid := say("локално"), say("облак")

	content, by, err := ask(t, routed(local, paid, true), context.Background(), generalQuestion)

	require.NoError(t, err)
	assert.Equal(t, "локално", content)
	assert.Equal(t, answeredBy{Provider: "local"}, by, "the client is told, so it can show it")
	assert.Empty(t, paid.Requests, "nothing leaves the machine that was not asked to")
}

func TestTheOwnersChoiceWinsEitherWay(t *testing.T) {
	t.Run("the cloud for a general question", func(t *testing.T) {
		local, paid := say("локално"), say("облак")

		content, by, err := ask(t, routed(local, paid, true), WithLLMChoice(context.Background(), yes()), generalQuestion)

		require.NoError(t, err)
		assert.Equal(t, "облак", content)
		assert.Equal(t, "deepseek", by.Provider)
		assert.Empty(t, local.Requests)
	})
	t.Run("home even for a question to the library", func(t *testing.T) {
		local, paid := say("локално"), say("облак")

		content, _, err := ask(t, routed(local, paid, true), WithLLMChoice(context.Background(), no()), libraryQuestion)

		require.NoError(t, err)
		assert.Equal(t, "локално", content)
		assert.Empty(t, paid.Requests)
	})
	t.Run("no choice is no choice", func(t *testing.T) {
		assert.Equal(t, context.Background(), WithLLMChoice(context.Background(), nil))
	})
}

func TestLibraryLLMLocalKeepsTheLibraryAtHomeButTheButtonStillWorks(t *testing.T) {
	local, paid := say("локално"), say("облак")
	o := routed(local, paid, false)

	content, _, err := ask(t, o, context.Background(), libraryQuestion)
	require.NoError(t, err)
	assert.Equal(t, "локално", content)

	content, _, err = ask(t, o, WithLLMChoice(context.Background(), yes()), libraryQuestion)
	require.NoError(t, err)
	assert.Equal(t, "облак", content)
}

// The cloud is down, the limit of the day is spent, the network is gone: the chat goes on.
func TestWhenThePaidModelFailsFirstTheLocalOneAnswers(t *testing.T) {
	for name, failure := range map[string]error{
		"the daily limit":  llm.ErrSpendCap,
		"the network":      errors.New("dial tcp: connection refused"),
		"a refusal of 402": errors.New("error, status code: 402, message: Insufficient Balance"),
	} {
		t.Run(name, func(t *testing.T) {
			local := say("локално")
			paid := &llm.MockLLM{Error: failure}
			before := routeCount("local", "fallback")

			content, by, err := ask(t, routed(local, paid, true), context.Background(), libraryQuestion)

			require.NoError(t, err)
			assert.Equal(t, "локално", content)
			assert.Equal(t, answeredBy{Provider: "local"}, by, "the client is not told the cloud answered")
			assert.EqualValues(t, 1, routeCount("local", "fallback")-before)
		})
	}
}

// After a tool has run the turn is not repeated: a second run would do the first one's writes again.
func TestAfterAToolHasRunTheTurnIsNotRepeatedOnTheLocalModel(t *testing.T) {
	local := say("локално")
	paid := &llm.MockLLM{
		StreamScript: [][]openai.ChatCompletionStreamResponse{{toolCallChunk(0, "c1", "echo", `{}`)}, {textChunk("never")}},
		StreamErrors: []error{nil, errors.New("the cloud went away")},
	}
	before := routeCount("paid_failed", "library")

	_, _, err := ask(t, routed(local, paid, true), context.Background(), libraryQuestion)

	require.Error(t, err)
	assert.Empty(t, local.Requests, "the local model does not do it again")
	assert.EqualValues(t, 1, routeCount("paid_failed", "library")-before)
}

func TestAFinishedRequestIsNotRetried(t *testing.T) {
	local := say("локално")
	paid := &llm.MockLLM{Error: context.Canceled}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := ask(t, routed(local, paid, true), ctx, libraryQuestion)

	require.Error(t, err)
	assert.Empty(t, local.Requests)
}

func TestTheStreamTellsWhoAnswered(t *testing.T) {
	local, paid := say("локално"), say("облак")
	o := routed(local, paid, true)
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", nil)
	ctx, _ := withTurn(req.Context(), "s", false)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	_, err := o.streamResponse(rec, req, "s",
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: libraryQuestion}}, toolDefinitions(), nil, nil)

	require.NoError(t, err)
	body := rec.Body.String()
	frame := `event: model` + "\n" + `data: {"provider":"deepseek","model":"deepseek-flash"}`
	require.Contains(t, body, frame)
	assert.Less(t, strings.Index(body, frame), strings.Index(body, "data: [DONE]"), "before the end of the stream")
}

func TestWithoutAPaidModelTheStreamIsWhatItAlwaysWas(t *testing.T) {
	o := &Orchestrator{llmClient: say("локално"), tools: &fakeToolRegistry{}}
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", nil)
	rec := httptest.NewRecorder()

	_, err := o.streamResponse(rec, req, "s",
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, nil, nil, nil)

	require.NoError(t, err)
	assert.NotContains(t, rec.Body.String(), "event: model")
}

func TestTheRequestsCarryTheChoice(t *testing.T) {
	var req ChatRequest
	require.NoError(t, jsonDecode(`{"session_id":"s","content":"c","use_paid":true}`, &req))
	require.NotNil(t, req.UsePaid)
	assert.True(t, *req.UsePaid)

	req = ChatRequest{}
	require.NoError(t, jsonDecode(`{"session_id":"s","content":"c"}`, &req))
	assert.Nil(t, req.UsePaid, "left out is no choice")

	var regen RegenerateRequest
	require.NoError(t, jsonDecode(`{"session_id":"s","use_paid":false}`, &regen))
	require.NotNil(t, regen.UsePaid)
	assert.False(t, *regen.UsePaid)
}

func jsonDecode(text string, into any) error { return json.Unmarshal([]byte(text), into) }

// The code review of 2026-11-27: a turn of Telegram (ProcessChatTurn) did not hand the choice of
// the model to the router, so TELEGRAM_LLM=local sent a question to the library to the cloud.
func TestATelegramTurnKeepsTheChoiceOfTheModel(t *testing.T) {
	for _, c := range []struct {
		name    string
		usePaid *bool
		cloud   bool
	}{{"no choice", nil, true}, {"local", no(), false}, {"cloud", yes(), true}} {
		t.Run(c.name, func(t *testing.T) {
			e := newAttachmentEnv(t)
			local := answeringModel()
			paid := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("облак")}}
			o := orchestratorWith(t, e, local).WithPaidModel(paid, "deepseek-flash", true)
			t.Cleanup(o.summaryWG.Wait)

			_, err := o.ProcessChatTurn(context.Background(), e.userID, ChatRequest{SessionID: "tg-1", Content: libraryQuestion, UsePaid: c.usePaid})

			require.NoError(t, err)
			assert.Equal(t, c.cloud, len(paid.Requests) > 0, "the cloud was asked")
			assert.Equal(t, !c.cloud, len(local.Requests) > 0, "the local model was asked")
		})
	}
}

// Work on many items is no question to the library: it stays on the local model (the owner, 2026-11-21),
// though its first round is forced too (the stub of the plan). On 2026-11-28 it went to the cloud.
func TestTheStubOfAPlanIsNoQuestionToTheLibrary(t *testing.T) {
	local, paid := say("локално"), say("облак")
	o := routed(local, paid, true)
	tools := append(toolDefinitions(), planStubDefinition())
	const task = "за всеки от файловете в папката си направи кратко резюме в нов отделен файл"
	require.Equal(t, planStubTool, forcedFirstTool(tools, task))

	use, reason := o.routeToPaid(context.Background(), []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: task}}, tools)
	assert.False(t, use)
	assert.Equal(t, "default", reason)

	use, _ = o.routeToPaid(context.Background(), []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: libraryQuestion}}, tools)
	assert.True(t, use, "a question to the books still goes to the cloud where LIBRARY_LLM lets it")
}
