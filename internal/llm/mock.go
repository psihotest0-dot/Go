package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"go-monolith/internal/config"
	"io"
	"net/http"
	"sync"

	openai "github.com/sashabaranov/go-openai"
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
//
// The orchestrator calls the model from more than one goroutine (a chat turn
// streams while the rolling summary of the same session runs in the
// background), so the calls record their request under a lock. A test reads
// LastRequest, LastSchema and Requests after the calls it cares about have
// returned (the summary pass is awaited with the orchestrator's summaryWG).
type MockLLM struct {
	mu          sync.Mutex
	LastRequest openai.ChatCompletionRequest
	LastSchema  interface{}
	// StreamChunks is the script used for every ChatStream call when
	// StreamScript is empty.
	StreamChunks []openai.ChatCompletionStreamResponse
	// StreamScript supplies the chunks for the n-th ChatStream call; the last
	// entry is reused for any further call. It takes precedence over
	// StreamChunks so multi-round conversations can be exercised.
	StreamScript [][]openai.ChatCompletionStreamResponse
	// StreamErrors makes the n-th ChatStream call fail when the entry is not
	// nil. Missing entries mean "no error".
	StreamErrors []error
	// Requests records every ChatStream request in call order.
	Requests []openai.ChatCompletionRequest
	Response openai.ChatCompletionResponse
	Error    error
}

// Chat records the request and returns the configured response or error.
func (m *MockLLM) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	m.mu.Lock()
	m.LastRequest = req
	m.mu.Unlock()
	return m.Response, m.Error
}

// ChatStream records the request and uses the mock round tripper to simulate a
// stream. StreamScript selects the chunks per call so multi-round conversations
// can be tested; the last scripted entry is reused for further calls.
func (m *MockLLM) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (Stream, error) {
	m.mu.Lock()
	m.LastRequest = req
	m.Requests = append(m.Requests, req)
	call := len(m.Requests) - 1
	m.mu.Unlock()

	if call < len(m.StreamErrors) && m.StreamErrors[call] != nil {
		return nil, m.StreamErrors[call]
	}
	if m.Error != nil {
		return nil, m.Error
	}

	chunks := m.StreamChunks
	if len(m.StreamScript) > 0 {
		if call < len(m.StreamScript) {
			chunks = m.StreamScript[call]
		} else {
			chunks = m.StreamScript[len(m.StreamScript)-1]
		}
	}

	config := openai.DefaultConfig("mock-token")
	config.HTTPClient = &http.Client{
		Transport: &MockRoundTripper{
			StreamChunks: chunks,
		},
	}
	config.EmptyMessagesLimit = 4294967295 // math.MaxUint32
	client := openai.NewClientWithConfig(config)

	// Since we mock the round tripper, we can just call the client normally
	stream, err := client.CreateChatCompletionStream(context.Background(), openai.ChatCompletionRequest{
		Model:  "mock",
		Stream: true,
	})
	if err != nil {
		// Returning the nil *ChatCompletionStream directly would give a non-nil
		// Stream interface wrapping a nil pointer.
		return nil, err
	}
	return stream, nil
}

// ChatWithJSONSchema records the request and schema, and returns the configured response or error.
func (m *MockLLM) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error) {
	m.mu.Lock()
	m.LastRequest = req
	m.LastSchema = schema
	m.mu.Unlock()
	return m.Response, m.Error
}

// MockEmbedder implements Embedder interface with deterministic vector generation.
type MockEmbedder struct{}

func (m *MockEmbedder) EmbedTexts(ctx context.Context, texts []string, isQuery bool) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	resultVectors := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, config.VectorDimension)
		h := sha256.Sum256([]byte(text))

		// Fill first 32 dimensions with hash values
		for j := 0; j < 32; j++ {
			vec[j] = float32(h[j]) / 255.0
		}

		// Fill rest to simulate something
		for j := 32; j < config.VectorDimension; j++ {
			vec[j] = float32(j) / float32(config.VectorDimension)
		}

		resultVectors[i] = vec
	}

	return resultVectors, nil
}

// Add simple config struct wrapper
type MockEmbedderWithConfig struct {
	EmptyVectors bool
	Dim          int
	Err          error
}

func (m *MockEmbedderWithConfig) EmbedTexts(ctx context.Context, texts []string, isQuery bool) ([][]float32, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	if m.EmptyVectors || len(texts) == 0 {
		return nil, nil
	}

	dim := m.Dim
	if dim == 0 {
		dim = config.VectorDimension
	}

	resultVectors := make([][]float32, len(texts))
	for i := range texts {
		vec := make([]float32, dim)
		// fill something
		for j := 0; j < dim; j++ {
			vec[j] = float32(j) / float32(dim)
		}
		resultVectors[i] = vec
	}

	return resultVectors, nil
}
