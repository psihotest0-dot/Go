package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	config.EmptyMessagesLimit = 4294967295 // math.MaxUint32

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
		return resp, err
	}

	maxRetries := 4
	baseDelay := 100 * time.Millisecond

	// go-openai Client doesn't export Config(), but NewLemonadeClient initializes it
	// We can recreate it or access it via an environment variable or default
	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:13305/v1"
	}
	url := baseURL + "/chat/completions"

	httpClient := &http.Client{
		Timeout: 0,
	}

	for i := 0; i < maxRetries; i++ {
		httpReq, reqErr := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
		if reqErr != nil {
			err = reqErr
			break
		}

		httpReq.Header.Set("Content-Type", "application/json")

		// If auth is needed, you could optionally fetch from env here
		// httpReq.Header.Set("Authorization", "Bearer ...")

		httpResp, httpErr := httpClient.Do(httpReq)
		if httpErr == nil {
			defer httpResp.Body.Close()
			if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
				respBody, readErr := io.ReadAll(httpResp.Body)
				if readErr == nil {
					unmarshalErr := json.Unmarshal(respBody, &resp)
					if unmarshalErr == nil {
						return resp, nil
					}
					err = unmarshalErr
				} else {
					err = readErr
				}
			} else {
				respBody, _ := io.ReadAll(httpResp.Body)
				err = errors.New("API error: " + httpResp.Status + " " + string(respBody))
			}
		} else {
			err = httpErr
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
