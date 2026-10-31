package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/llm"
)

// An LLM failure can embed the provider URL or response body. The client must
// only see a generic message plus the request id used to find it in the log.
func TestStreamResponseHidesInternalErrorDetails(t *testing.T) {
	internal := errors.New(`Post "http://10.1.2.3:13305/v1/chat/completions": dial tcp 10.1.2.3:13305: connectex: refused`)
	orchestrator := NewOrchestrator(nil, nil, &llm.MockLLM{Error: internal}, nil)

	req := httptest.NewRequest(http.MethodPost, "/chat/stream", nil)
	req = req.WithContext(context.WithValue(req.Context(), api.RequestIDKey, "req-123"))
	rec := httptest.NewRecorder()

	_, _, err := orchestrator.streamResponse(rec, req, "sess-1",
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}, nil, nil)

	require.Error(t, err)
	body := rec.Body.String()
	require.Contains(t, body, "event: error")
	require.Contains(t, body, "request_id: req-123")
	require.NotContains(t, body, "10.1.2.3")
	require.NotContains(t, body, "13305")
	require.NotContains(t, body, "connectex")
}

func TestStreamErrorMessageWithoutRequestID(t *testing.T) {
	require.NotContains(t, streamErrorMessage(""), "request_id")
}

func TestStreamFilterKeepsBufferBoundedOnUnterminatedTag(t *testing.T) {
	filter := &streamFilter{}
	input := strings.Repeat("<", 512) + "not a tag"

	out := filter.Process(input)

	require.LessOrEqual(t, len(filter.buffer), streamFilterMaxBuffered, "buffer must stay bounded")
	require.Equal(t, input, out+filter.Flush())
	require.Empty(t, filter.buffer)
}

func TestStreamFilterResetReleasesBuffer(t *testing.T) {
	filter := &streamFilter{}

	_ = filter.Process("<thi")
	require.NotEmpty(t, filter.buffer)

	filter.Reset()
	require.Empty(t, filter.buffer)
	require.False(t, filter.inThink)

	_ = filter.Process("<think>hidden reasoning")
	require.True(t, filter.inThink)
	require.Empty(t, filter.Process("more hidden reasoning"))

	filter.Reset()
	require.False(t, filter.inThink)
	require.Empty(t, filter.buffer)
}
