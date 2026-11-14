package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/metrics"
)

func counter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

// A reply the way DeepSeek writes it: the usage tells which input came from its cache.
const deepSeekReply = `{"id":"1","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"{\"verdict\":\"HOLD\",\"reason\":\"x\"}"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_cache_hit_tokens":300,"prompt_cache_miss_tokens":700}}`

func TestDeepSeekRequestBody(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"top_k":40,"temperature":0.2,` +
		`"chat_template_kwargs":{"enable_thinking":false},` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`

	out, err := deepSeekRequestBody([]byte(in))

	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(out, &body))
	assert.NotContains(t, body, "chat_template_kwargs", "only llama-server knows it")
	assert.NotContains(t, body, "top_k")
	assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"], "thinking is on by default and then needs reasoning_content back")
	assert.Equal(t, map[string]any{"type": "json_object"}, body["response_format"])
	assert.Equal(t, "m", body["model"])
	assert.EqualValues(t, 0.2, body["temperature"], "the rest goes on as it was")
}

func TestDeepSeekRequestBodyLeavesAPlainRequestAlone(t *testing.T) {
	out, err := deepSeekRequestBody([]byte(`{"model":"m","messages":[],"stream":true}`))

	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(out, &body))
	assert.NotContains(t, body, "response_format")
	assert.Equal(t, true, body["stream"])
}

func TestDeepSeekRequestBodyRefusesWhatIsNotJSON(t *testing.T) {
	_, err := deepSeekRequestBody([]byte(`not json`))
	assert.Error(t, err)
}

// What goes over the wire to the paid API: the key in the header, the dialect in the body,
// the model pinned. The server here stands for api.deepseek.com.
func TestDeepSeekClientSpeaksTheDialectAndCountsTheTokens(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	c := newTestClient(t, Options{Dialect: DialectDeepSeek, APIKey: "the-key", Model: "deepseek-flash", ForceModel: true},
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(raw, &gotBody))
			writeJSON(w, http.StatusOK, deepSeekReply)
		})
	hit := counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_hit"))
	miss := counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_miss"))
	out := counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "output"))
	ok := counter(t, metrics.PaidLLMRequests.WithLabelValues("deepseek", "2xx"))

	type answer struct {
		Verdict string `json:"verdict"`
	}
	resp, err := c.ChatWithJSONSchema(context.Background(), userRequest("hi"), answer{})

	require.NoError(t, err)
	assert.Equal(t, "Bearer the-key", gotAuth)
	assert.Equal(t, "deepseek-flash", gotBody["model"])
	assert.Equal(t, map[string]any{"type": "disabled"}, gotBody["thinking"])
	assert.NotContains(t, gotBody, "chat_template_kwargs")
	assert.Equal(t, map[string]any{"type": "json_object"}, gotBody["response_format"])
	assert.Equal(t, "deepseek-flash", resp.Model, "the journal of the Oracle records this name")
	assert.EqualValues(t, 300, counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_hit"))-hit)
	assert.EqualValues(t, 700, counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_miss"))-miss)
	assert.EqualValues(t, 50, counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "output"))-out)
	assert.EqualValues(t, 1, counter(t, metrics.PaidLLMRequests.WithLabelValues("deepseek", "2xx"))-ok)
}

// The typed requests of the chat go through the same dialect, and a stream is counted from its last chunk.
func TestDeepSeekStreamIsRewrittenAndCounted(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, Options{Dialect: DialectDeepSeek, APIKey: "k", Model: "deepseek-flash", ForceModel: true},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(raw, &gotBody))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"+
				"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12,\"prompt_cache_hit_tokens\":6,\"prompt_cache_miss_tokens\":4}}\n\n"+
				"data: [DONE]\n\n")
		})
	hit := counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_hit"))

	req := userRequest("hi")
	req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	stream, err := c.ChatStream(context.Background(), req)
	require.NoError(t, err)
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	require.NoError(t, stream.Close())

	assert.NotContains(t, gotBody, "chat_template_kwargs")
	assert.Equal(t, map[string]any{"type": "disabled"}, gotBody["thinking"])
	assert.EqualValues(t, 6, counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_hit"))-hit)
}

func TestDeepSeekErrorIsCountedByItsClass(t *testing.T) {
	c := newTestClient(t, Options{Dialect: DialectDeepSeek, APIKey: "k", Model: "deepseek-flash", ForceModel: true},
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusUnauthorized, `{"error":{"message":"Authentication Fails"}}`)
		})
	before := counter(t, metrics.PaidLLMRequests.WithLabelValues("deepseek", "4xx"))

	_, err := c.Chat(context.Background(), userRequest("hi"))

	require.Error(t, err)
	assert.EqualValues(t, 1, counter(t, metrics.PaidLLMRequests.WithLabelValues("deepseek", "4xx"))-before)
	assert.NotContains(t, err.Error(), "k\"", "the key is not in the error")
}

// The local client is what it was: the dummy key, the caller's request as it is, nothing counted as paid.
func TestLocalClientIsUnchangedByTheDialect(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		writeJSON(w, http.StatusOK, deepSeekReply)
	})
	hit := counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_hit"))

	req := userRequest("hi")
	req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	_, err := c.Chat(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, "Bearer lemonade-dummy-key", gotAuth)
	assert.Contains(t, gotBody, "chat_template_kwargs")
	assert.NotContains(t, gotBody, "thinking")
	assert.Zero(t, counter(t, metrics.PaidLLMTokens.WithLabelValues("deepseek", "cache_hit"))-hit, "a local reply is not paid")
}

func TestNewDeepSeekClientDefaultsToTheCheapModel(t *testing.T) {
	c := NewDeepSeekClient("k", "", 1)
	assert.Equal(t, DefaultDeepSeekModel, c.model)
	assert.True(t, c.forceModel)
	assert.Equal(t, DeepSeekBaseURL, c.baseURL)

	assert.Equal(t, "deepseek-v4-pro", NewDeepSeekClient("k", "deepseek-v4-pro", 1).model)
}

func TestCountPaidTokensTakesTheLastUsageOfTheText(t *testing.T) {
	before := counter(t, metrics.PaidLLMTokens.WithLabelValues("test-provider", "output"))

	countPaidTokens("test-provider", []byte(strings.Repeat("x", 100)+`"completion_tokens": 7, "completion_tokens": 9`))

	assert.EqualValues(t, 9, counter(t, metrics.PaidLLMTokens.WithLabelValues("test-provider", "output"))-before)
}
