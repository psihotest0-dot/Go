package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/sashabaranov/go-openai"
)

// MockRoundTripper is a custom http.RoundTripper that simulates responses
// without spinning up a real HTTP server.
type MockRoundTripper struct {
	StreamChunks []openai.ChatCompletionStreamResponse
	Response     openai.ChatCompletionResponse
	Error        error
}

func (m *MockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if m.Error != nil {
		return nil, m.Error
	}

	// Determine if it's a streaming request based on the request body
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // restore for later
	}

	var chatReq openai.ChatCompletionRequest
	json.Unmarshal(bodyBytes, &chatReq)

	if chatReq.Stream {
		// Simulate SSE stream
		var buf bytes.Buffer
		for _, chunk := range m.StreamChunks {
			data, _ := json.Marshal(chunk)
			buf.WriteString("data: ")
			buf.Write(data)
			buf.WriteString("\n\n")
		}
		buf.WriteString("data: [DONE]\n\n")

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&buf),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}, nil
	}

	// Simulate regular JSON response
	respBytes, _ := json.Marshal(m.Response)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBuffer(respBytes)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// MockLLM is a mock implementation of LLMClient for testing.
type MockLLM struct {
	LastRequest  openai.ChatCompletionRequest
	LastSchema   interface{}
	StreamChunks []openai.ChatCompletionStreamResponse
	Response     openai.ChatCompletionResponse
	Error        error
}

// Chat records the request and returns the configured response or error.
func (m *MockLLM) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	m.LastRequest = req
	return m.Response, m.Error
}

// ChatStream records the request and uses the mock round tripper to simulate a stream.
func (m *MockLLM) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error) {
	m.LastRequest = req

	if m.Error != nil {
		return nil, m.Error
	}

	config := openai.DefaultConfig("mock-token")
	config.HTTPClient = &http.Client{
		Transport: &MockRoundTripper{
			StreamChunks: m.StreamChunks,
		},
	}
	client := openai.NewClientWithConfig(config)

	// Since we mock the round tripper, we can just call the client normally
	return client.CreateChatCompletionStream(context.Background(), openai.ChatCompletionRequest{
		Model:  "mock",
		Stream: true,
	})
}

// ChatWithJSONSchema records the request and schema, and returns the configured response or error.
func (m *MockLLM) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error) {
	m.LastRequest = req
	m.LastSchema = schema
	return m.Response, m.Error
}
