package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"

	"go-monolith/metrics"
)

// LLMClient defines the interface for interacting with the LLM provider.
type LLMClient interface {
	Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
	ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error)
	ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error)
}

// LemonadeClient implements LLMClient using the standard OpenAI format.
type LemonadeClient struct {
	client      *openai.Client
	cachedModel string
	cacheTime   time.Time
	mu          sync.RWMutex
}

func (c *LemonadeClient) getDynamicModel(ctx context.Context) string {
	c.mu.RLock()
	if time.Since(c.cacheTime) < 5*time.Minute && c.cachedModel != "" {
		defer c.mu.RUnlock()
		return c.cachedModel
	}
	c.mu.RUnlock()

	models, err := c.client.ListModels(ctx)
	if err == nil && len(models.Models) > 0 {
		c.mu.Lock()
		c.cachedModel = models.Models[0].ID
		c.cacheTime = time.Now()
		defer c.mu.Unlock()
		return c.cachedModel
	}

	return "lemonade"
}

// NewLemonadeClient creates a new client connected to the Lemonade server.
func NewLemonadeClient() *LemonadeClient {
	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:13305/v1"
	}

	config := openai.DefaultConfig("lemonade-dummy-key")
	config.BaseURL = baseURL

	httpClient := &http.Client{
		Timeout: 0,
	}
	config.HTTPClient = httpClient

	client := openai.NewClientWithConfig(config)
	return &LemonadeClient{client: client}
}

func (c *LemonadeClient) setModel(ctx context.Context, req *openai.ChatCompletionRequest) {
	provider := os.Getenv("LLM_PROVIDER")
	if provider == "lemonade" || req.Model == "" || req.Model == "lemonade" {
		req.Model = c.getDynamicModel(ctx)
	}
}

func (c *LemonadeClient) Chat(ctx context.Context, req openai.ChatCompletionRequest) (resp openai.ChatCompletionResponse, err error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("llm").Observe(time.Since(start).Seconds())
		if err == nil {
			metrics.TokenCount.WithLabelValues("prompt").Add(float64(resp.Usage.PromptTokens))
			metrics.TokenCount.WithLabelValues("completion").Add(float64(resp.Usage.CompletionTokens))
			metrics.TokenCount.WithLabelValues("total").Add(float64(resp.Usage.TotalTokens))
		}
	}()

	c.setModel(ctx, &req)

	maxRetries := 4 // 1 initial + 3 retries
	baseDelay := 100 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		resp, err = c.client.CreateChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}

		if i == maxRetries-1 {
			break
		}

		delay := baseDelay * (1 << i)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return resp, ctx.Err()
		}
	}

	return resp, errors.New("max retries exceeded: " + err.Error())
}

func (c *LemonadeClient) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (stream *openai.ChatCompletionStream, err error) {
	c.setModel(ctx, &req)
	req.Stream = true

	maxRetries := 4
	baseDelay := 100 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		stream, err = c.client.CreateChatCompletionStream(ctx, req)
		if err == nil {
			return stream, nil
		}

		if i == maxRetries-1 {
			break
		}

		delay := baseDelay * (1 << i)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, errors.New("max retries exceeded for stream: " + err.Error())
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

func (c *LemonadeClient) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (resp openai.ChatCompletionResponse, err error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("llm").Observe(time.Since(start).Seconds())
		if err == nil {
			metrics.TokenCount.WithLabelValues("prompt").Add(float64(resp.Usage.PromptTokens))
			metrics.TokenCount.WithLabelValues("completion").Add(float64(resp.Usage.CompletionTokens))
			metrics.TokenCount.WithLabelValues("total").Add(float64(resp.Usage.TotalTokens))
		}
	}()

	c.setModel(ctx, &req)

	req.ResponseFormat = &openai.ChatCompletionResponseFormat{
		Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
		JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
			Name:   "structured_output",
			Schema: schemaWrapper{v: schema},
			Strict: true,
		},
	}

	maxRetries := 4
	baseDelay := 100 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		resp, err = c.client.CreateChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}

		if i == maxRetries-1 {
			break
		}

		delay := baseDelay * (1 << i)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return resp, ctx.Err()
		}
	}

	return resp, errors.New("max retries exceeded for json schema: " + err.Error())
}
