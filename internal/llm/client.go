package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"

	"go-monolith/internal/config"
	"go-monolith/metrics"
)

const (
	defaultBaseURL = "http://localhost:13305/v1"

	// fallbackModel is sent when the server cannot tell which model it serves.
	fallbackModel = "lemonade"

	// modelCacheTTL is how long a discovered model name is trusted;
	// modelFailureTTL is how long a failed discovery is remembered, so an
	// unreachable server does not cost an extra round trip on every request.
	modelCacheTTL   = 5 * time.Minute
	modelFailureTTL = 30 * time.Second
)

// Stream is what callers need from a streaming completion. It is an interface
// rather than *openai.ChatCompletionStream so the client can hold the model slot
// until the stream is finished.
type Stream interface {
	Recv() (openai.ChatCompletionStreamResponse, error)
	Close() error
}

// LLMClient defines the interface for interacting with the LLM provider.
type LLMClient interface {
	Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
	ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (Stream, error)
	ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error)
}

// Options configures a LemonadeClient.
type Options struct {
	// BaseURL of the OpenAI compatible API; empty means the local Lemonade.
	BaseURL string
	// Model pins the model name. Empty means "ask the server".
	Model string
	// ForceModel replaces a model name chosen by the caller too (LLM_PROVIDER=lemonade).
	ForceModel bool
	// Slots is the number of completions the server runs in parallel and
	// BackgroundSlots how many of them background work may occupy.
	Slots           int
	BackgroundSlots int
}

// LemonadeClient implements LLMClient using the standard OpenAI format. Every
// request takes one of the server's parallel slots for as long as it runs, so
// the application never sends more work than the server can run at once.
type LemonadeClient struct {
	client     *openai.Client
	httpClient *http.Client
	baseURL    string
	model      string
	forceModel bool
	limiter    *slotLimiter

	retryBaseDelay time.Duration

	mu              sync.Mutex
	discoveredModel string
	discoveredUntil time.Time
}

// NewLemonadeClient creates a client configured from the environment
// (LLM_BASE_URL, LLM_MODEL, LLM_SLOTS, LLM_BACKGROUND_SLOTS, LLM_PROVIDER).
func NewLemonadeClient() *LemonadeClient {
	cfg := config.Load()
	return NewLemonadeClientWithOptions(Options{
		BaseURL:         cfg.LLMBaseURL,
		Model:           cfg.LLMModel,
		ForceModel:      os.Getenv("LLM_PROVIDER") == "lemonade",
		Slots:           cfg.LLMSlots,
		BackgroundSlots: cfg.LLMBackgroundSlots,
	})
}

// NewLemonadeClientWithOptions creates a client connected to the Lemonade server.
func NewLemonadeClientWithOptions(opts Options) *LemonadeClient {
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if opts.Slots < 1 {
		opts.Slots = config.DefaultLLMSlots
	}
	if opts.BackgroundSlots < 1 {
		opts.BackgroundSlots = config.DefaultLLMBackgroundSlots
	}

	httpClient := &http.Client{
		Timeout: 0,
	}

	clientConfig := openai.DefaultConfig("lemonade-dummy-key")
	clientConfig.BaseURL = baseURL
	clientConfig.HTTPClient = httpClient
	clientConfig.EmptyMessagesLimit = 4294967295 // math.MaxUint32

	return &LemonadeClient{
		client:         openai.NewClientWithConfig(clientConfig),
		httpClient:     httpClient,
		baseURL:        baseURL,
		model:          strings.TrimSpace(opts.Model),
		forceModel:     opts.ForceModel,
		limiter:        newSlotLimiter(opts.Slots, opts.BackgroundSlots),
		retryBaseDelay: defaultRetryBaseDelay,
	}
}

// resolveModel returns the model name to send: the pinned one, otherwise the one
// the server reports (cached, also when the lookup failed).
func (c *LemonadeClient) resolveModel(ctx context.Context) string {
	if c.model != "" {
		return c.model
	}

	c.mu.Lock()
	if time.Now().Before(c.discoveredUntil) {
		model := c.discoveredModel
		c.mu.Unlock()
		return model
	}
	c.mu.Unlock()

	model, ttl := fallbackModel, modelFailureTTL
	models, err := c.client.ListModels(ctx)
	switch {
	case err == nil && len(models.Models) > 0:
		// The server's order is not guaranteed to be stable, so pick by name.
		ids := make([]string, 0, len(models.Models))
		for _, m := range models.Models {
			ids = append(ids, m.ID)
		}
		slices.Sort(ids)
		model, ttl = ids[0], modelCacheTTL
		if len(ids) > 1 {
			slog.Warn("LLM server lists several models and LLM_MODEL is not set; using the first by name",
				"model", model, "available", ids)
		}
	case ctx.Err() != nil:
		// The caller gave up; that says nothing about the server, so do not cache it.
		return fallbackModel
	case err != nil:
		slog.Warn("could not ask the LLM server for its model", "error", err, "using", fallbackModel)
	default:
		slog.Warn("LLM server reported no models", "using", fallbackModel)
	}

	c.mu.Lock()
	c.discoveredModel = model
	c.discoveredUntil = time.Now().Add(ttl)
	c.mu.Unlock()
	return model
}

func (c *LemonadeClient) setModel(ctx context.Context, req *openai.ChatCompletionRequest) {
	if c.forceModel || req.Model == "" || req.Model == fallbackModel {
		req.Model = c.resolveModel(ctx)
	}
}

// countTokens adds the usage reported by the server to the token counters.
func countTokens(usage openai.Usage) {
	metrics.TokenCount.WithLabelValues("prompt").Add(float64(usage.PromptTokens))
	metrics.TokenCount.WithLabelValues("completion").Add(float64(usage.CompletionTokens))
	metrics.TokenCount.WithLabelValues("total").Add(float64(usage.TotalTokens))
}

// observe records latency and token usage of a finished call.
func observe(start time.Time, resp openai.ChatCompletionResponse, err error) {
	metrics.StageLatency.WithLabelValues("llm").Observe(time.Since(start).Seconds())
	if err == nil {
		countTokens(resp.Usage)
	}
}

func (c *LemonadeClient) Chat(ctx context.Context, req openai.ChatCompletionRequest) (resp openai.ChatCompletionResponse, err error) {
	start := time.Now()
	defer func() { observe(start, resp, err) }()

	c.setModel(ctx, &req)

	err = retry(ctx, "chat", c.retryBaseDelay, func() error {
		release, acquireErr := c.limiter.acquire(ctx, IsBackground(ctx))
		if acquireErr != nil {
			return acquireErr
		}
		defer release()

		var callErr error
		resp, callErr = c.client.CreateChatCompletion(ctx, req)
		return callErr
	})
	return resp, err
}

// leasedStream keeps the model slot until the stream is finished: on EOF or on
// Close, whichever comes first.
type leasedStream struct {
	stream  *openai.ChatCompletionStream
	release func()
}

func (s *leasedStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	resp, err := s.stream.Recv()
	if errors.Is(err, io.EOF) {
		s.release()
	}
	if err == nil && resp.Usage != nil {
		countTokens(*resp.Usage)
	}
	return resp, err
}

func (s *leasedStream) Close() error {
	err := s.stream.Close()
	s.release()
	return err
}

// ChatStream starts a streaming completion. The slot stays taken until the
// caller reads the stream to its end or closes it, so callers must Close it.
func (c *LemonadeClient) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (Stream, error) {
	c.setModel(ctx, &req)
	req.Stream = true
	if req.StreamOptions == nil {
		// Without this a stream carries no token counts, so they could neither
		// be logged nor counted.
		req.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}

	var leased *leasedStream
	err := retry(ctx, "stream", c.retryBaseDelay, func() error {
		release, acquireErr := c.limiter.acquire(ctx, IsBackground(ctx))
		if acquireErr != nil {
			return acquireErr
		}

		stream, callErr := c.client.CreateChatCompletionStream(ctx, req)
		if callErr != nil {
			release()
			return callErr
		}
		leased = &leasedStream{stream: stream, release: release}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return leased, nil
}

// schemaWrapper allows us to pass a custom struct as a json.Marshaler.
type schemaWrapper struct {
	v interface{}
}

func (w schemaWrapper) MarshalJSON() ([]byte, error) {
	schema, err := jsonschema.GenerateSchemaForType(w.v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(schema)
}

// postChat sends one raw chat completion request and decodes the answer. A
// non-2xx answer is returned as *StatusError.
func (c *LemonadeClient) postChat(ctx context.Context, body []byte) (openai.ChatCompletionResponse, error) {
	var resp openai.ChatCompletionResponse

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return resp, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBodyBytes))
		return resp, &StatusError{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Body:       strings.TrimSpace(string(errBody)),
		}
	}

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return resp, fmt.Errorf("decode LLM response: %w", err)
	}
	return resp, nil
}

func (c *LemonadeClient) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (resp openai.ChatCompletionResponse, err error) {
	start := time.Now()
	defer func() { observe(start, resp, err) }()

	c.setModel(ctx, &req)

	bodyMap := map[string]interface{}{
		"model":       req.Model,
		"messages":    req.Messages,
		"max_tokens":  req.MaxTokens,
		"temperature": req.Temperature,
		"chat_template_kwargs": map[string]interface{}{
			"enable_thinking": false,
		},
		"response_format": map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name":   "structured_output",
				"schema": schemaWrapper{v: schema},
				"strict": true,
			},
		},
	}

	bodyBytes, err := json.Marshal(bodyMap)
	if err != nil {
		return resp, fmt.Errorf("llm json schema: %w", err)
	}

	err = retry(ctx, "json schema", c.retryBaseDelay, func() error {
		release, acquireErr := c.limiter.acquire(ctx, IsBackground(ctx))
		if acquireErr != nil {
			return acquireErr
		}
		defer release()

		var callErr error
		resp, callErr = c.postChat(ctx, bodyBytes)
		return callErr
	})
	return resp, err
}
