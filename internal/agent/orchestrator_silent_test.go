package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

// step is one thing a scripted stream does: wait, then send a chunk, or (block)
// go silent until the request is cancelled, the way llama-server does while
// Gemma 4 repeats its empty thought marker.
type step struct {
	wait    time.Duration
	content string
	block   bool
}

type scriptedStream struct {
	ctx   context.Context
	steps []step
}

func (s *scriptedStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if len(s.steps) == 0 {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	if st.block {
		<-s.ctx.Done()
		return openai.ChatCompletionStreamResponse{}, s.ctx.Err()
	}
	select {
	case <-time.After(st.wait):
	case <-s.ctx.Done():
		return openai.ChatCompletionStreamResponse{}, s.ctx.Err()
	}
	return openai.ChatCompletionStreamResponse{Choices: []openai.ChatCompletionStreamChoice{
		{Delta: openai.ChatCompletionStreamChoiceDelta{Role: "assistant", Content: st.content}},
	}}, nil
}

func (s *scriptedStream) Close() error { return nil }

// scriptedLLM plays one script per ChatStream call and remembers the requests.
type scriptedLLM struct {
	mu       sync.Mutex
	scripts  [][]step
	requests []openai.ChatCompletionRequest
}

func (m *scriptedLLM) Chat(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return openai.ChatCompletionResponse{}, errors.New("not used")
}

func (m *scriptedLLM) ChatWithJSONSchema(context.Context, openai.ChatCompletionRequest, interface{}) (openai.ChatCompletionResponse, error) {
	return openai.ChatCompletionResponse{}, errors.New("not used")
}

func (m *scriptedLLM) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (llm.Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	i := len(m.requests) - 1
	if i >= len(m.scripts) {
		return nil, errors.New("no script left")
	}
	return &scriptedStream{ctx: ctx, steps: append([]step(nil), m.scripts[i]...)}, nil
}

func (m *scriptedLLM) thinkingOf(i int) bool {
	on, _ := m.requests[i].ChatTemplateKwargs["enable_thinking"].(bool)
	return on
}

func shortSilence(t *testing.T) {
	t.Helper()
	old := silentStreamTimeout
	silentStreamTimeout = 50 * time.Millisecond
	t.Cleanup(func() { silentStreamTimeout = old })
}

// thinkingOff is an orchestrator configured like the live one: thinking off (the
// built-in legacy budgets used by bare test orchestrators have it on).
func thinkingOff(model llm.LLMClient) *Orchestrator {
	b := legacyBudgets()
	b.thinking = false
	return &Orchestrator{llmClient: model, budgets: b}
}

func generate(t *testing.T, o *Orchestrator) (string, error) {
	t.Helper()
	answer, _, _, err := o.generateResponse(context.Background(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Здравей"}}, nil, nil)
	return answer, err
}

// The loop of 2026-11-13: the stream starts and then sends nothing. It is cut off
// after silentStreamTimeout and the turn is asked again, once, with thinking on.
func TestASilentStreamIsAskedAgainWithThinkingOn(t *testing.T) {
	shortSilence(t)
	before := counterValue(t, metrics.LLMSilentStreams.WithLabelValues("retried"))
	model := &scriptedLLM{scripts: [][]step{
		{{content: ""}, {block: true}},
		{{content: "Отговор"}},
	}}
	o := thinkingOff(model)

	start := time.Now()
	answer, err := generate(t, o)

	require.NoError(t, err)
	require.Equal(t, "Отговор", answer)
	require.Len(t, model.requests, 2)
	require.False(t, model.thinkingOf(0))
	require.True(t, model.thinkingOf(1), "the second request must turn thinking on")
	require.Less(t, time.Since(start), 2*time.Second, "the silence must be cut off, not waited out")
	require.Equal(t, before+1, counterValue(t, metrics.LLMSilentStreams.WithLabelValues("retried")))
}

// Text the user already sees is not thrown away: it becomes the answer, and the
// turn is not asked again (that would print a second answer after the first).
func TestASilentStreamAfterSomeTextKeepsTheText(t *testing.T) {
	shortSilence(t)
	model := &scriptedLLM{scripts: [][]step{
		{{content: "Първата част."}, {block: true}},
	}}
	o := thinkingOff(model)

	answer, err := generate(t, o)

	require.NoError(t, err)
	require.Equal(t, "Първата част.", answer)
	require.Len(t, model.requests, 1)
}

// With thinking already on there is nothing left to try: the error goes to the
// caller (the client gets an error frame and can regenerate).
func TestASilentStreamWithThinkingOnIsAnError(t *testing.T) {
	shortSilence(t)
	model := &scriptedLLM{scripts: [][]step{
		{{content: ""}, {block: true}},
	}}
	b := legacyBudgets()
	b.thinking = true
	o := &Orchestrator{llmClient: model, budgets: b}

	_, err := generate(t, o)

	require.ErrorIs(t, err, errSilentStream)
	require.Len(t, model.requests, 1)
}

// The clock starts with the first chunk: reading a long prompt (here longer than
// the timeout) before anything arrives is not silence.
func TestReadingALongPromptIsNotSilence(t *testing.T) {
	shortSilence(t)
	model := &scriptedLLM{scripts: [][]step{
		{{wait: 150 * time.Millisecond, content: ""}, {wait: 10 * time.Millisecond, content: "Готово"}},
	}}
	o := thinkingOff(model)

	answer, err := generate(t, o)

	require.NoError(t, err)
	require.Equal(t, "Готово", answer)
	require.Len(t, model.requests, 1)
}

// A cancelled request (the client left) is not mistaken for silence and not retried.
func TestACancelledRequestIsNotRetried(t *testing.T) {
	shortSilence(t)
	model := &scriptedLLM{scripts: [][]step{
		{{content: ""}, {block: true}},
		{{content: "не бива"}},
	}}
	o := thinkingOff(model)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)

	_, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Здравей"}}, nil, nil)

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, model.requests, 1)
}
