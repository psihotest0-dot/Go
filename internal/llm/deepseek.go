package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"go-monolith/metrics"
)

// DeepSeek as the model of a purpose (so far the Oracle; ORACLE_LLM=deepseek). The API is
// OpenAI compatible, but a few things of the local server's dialect are not understood and a
// few of its own matter, found by measuring it on 2026-11-14:
//
//   - thinking is ON by default, and with tools in the request the next request must carry
//     reasoning_content or it is a 400, which this application does not keep - so the request
//     says {"thinking": {"type": "disabled"}} (and it is cheaper: no hidden output tokens);
//   - chat_template_kwargs and top_k are only for llama-server and are left out;
//   - the strict json_schema output of the local server is asked as json_object (the prompt
//     of the caller must name the JSON it wants, as the Oracle's does);
//   - the reply of the API tells how many input tokens came from its cache, which is what
//     the money is counted from (paid_llm_tokens_total).
const (
	// DeepSeekBaseURL is the API of DeepSeek.
	DeepSeekBaseURL = "https://api.deepseek.com"
	// DefaultDeepSeekModel is the cheap model; the other one is deepseek-v4-pro.
	DefaultDeepSeekModel = "deepseek-flash"

	// deepSeekProvider is the label of the metrics.
	deepSeekProvider = "deepseek"

	// usageTail is how much of the end of a reply is kept to read the usage from: in a
	// stream it is in the last chunk, in a plain reply after the choices.
	usageTail = 16 << 10
)

// Dialect says which server is behind the client.
type Dialect int

const (
	// DialectLocal is llama-server behind Lemonade: the request goes out as the caller built it.
	DialectLocal Dialect = iota
	// DialectDeepSeek is the paid API of DeepSeek.
	DialectDeepSeek
)

// NewDeepSeekClient returns a client of the DeepSeek API for the model (empty: the cheap
// one). The key is sent only in the Authorization header and is never logged.
//
// dailyUSD is the most the client may cost in a day (at the peak prices); at it the client
// refuses requests with ErrSpendCap. The guard is client.Spend().
func NewDeepSeekClient(apiKey, model string, dailyUSD float64) *LemonadeClient {
	if model == "" {
		model = DefaultDeepSeekModel
	}
	return NewLemonadeClientWithOptions(Options{
		BaseURL:    DeepSeekBaseURL,
		Model:      model,
		ForceModel: true, // a name chosen elsewhere is a model of the local server
		APIKey:     apiKey,
		Dialect:    DialectDeepSeek,
		Spend:      NewSpendGuard(dailyUSD),
		// A cloud API runs many requests at once; the slots only keep this process modest.
		Slots:           8,
		BackgroundSlots: 2,
	})
}

// deepSeekRequestBody turns a request built for the local server into one DeepSeek accepts.
func deepSeekRequestBody(raw []byte) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	delete(body, "chat_template_kwargs")
	delete(body, "top_k")
	body["thinking"] = map[string]any{"type": "disabled"}
	if format, ok := body["response_format"].(map[string]any); ok && format["type"] == "json_schema" {
		body["response_format"] = map[string]any{"type": "json_object"}
	}
	return json.Marshal(body)
}

// deepSeekTransport rewrites the chat requests to the dialect of DeepSeek and counts the
// tokens of the replies.
type deepSeekTransport struct {
	base  http.RoundTripper
	spend *SpendGuard
}

func newDeepSeekTransport(spend *SpendGuard) *deepSeekTransport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	// A reply that has not begun in a minute is not coming; a stream may then run as long as it likes.
	base.ResponseHeaderTimeout = time.Minute
	return &deepSeekTransport{base: base, spend: spend}
}

func (t *deepSeekTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	model := ""
	if req.Body != nil && req.Method == http.MethodPost {
		// The limit is checked before anything is sent: the request that would cross it is not made.
		if t.spend != nil {
			if err := t.spend.Allow(); err != nil {
				_ = req.Body.Close()
				return nil, err
			}
		}
		raw, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body, err := deepSeekRequestBody(raw)
		if err != nil {
			return nil, err
		}
		model = requestModel(body)
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.ContentLength = int64(len(body))
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		metrics.PaidLLMRequests.WithLabelValues(deepSeekProvider, "network_error").Inc()
		return resp, err
	}
	metrics.PaidLLMRequests.WithLabelValues(deepSeekProvider, statusClass(resp.StatusCode)).Inc()
	resp.Body = &usageBody{rc: resp.Body, model: model, spend: t.spend}
	return resp, nil
}

// requestModel is the model named in a request body ("" if it names none).
func requestModel(body []byte) string {
	var named struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &named)
	return named.Model
}

func statusClass(code int) string {
	return strconv.Itoa(code/100) + "xx"
}

// usageBody passes the reply on and, when it is closed, counts the tokens it reports.
type usageBody struct {
	rc    io.ReadCloser
	model string
	spend *SpendGuard
	tail  []byte
	once  sync.Once
}

func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.tail = append(b.tail, p[:n]...)
		if len(b.tail) > usageTail {
			b.tail = b.tail[len(b.tail)-usageTail:]
		}
	}
	return n, err
}

func (b *usageBody) Close() error {
	b.once.Do(func() {
		hit, miss, out := countPaidTokens(deepSeekProvider, b.tail)
		if b.spend != nil {
			b.spend.Add(EstimateCost(b.model, hit, miss, out))
		}
	})
	return b.rc.Close()
}

var (
	usageHit    = regexp.MustCompile(`"prompt_cache_hit_tokens"\s*:\s*(\d+)`)
	usageMiss   = regexp.MustCompile(`"prompt_cache_miss_tokens"\s*:\s*(\d+)`)
	usageOutput = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
)

// lastNumber is the last value of the field in the text (0 when it is not there).
func lastNumber(re *regexp.Regexp, text []byte) float64 {
	all := re.FindAllSubmatch(text, -1)
	if len(all) == 0 {
		return 0
	}
	n, _ := strconv.ParseFloat(string(all[len(all)-1][1]), 64)
	return n
}

// countPaidTokens adds what a reply says it used to the counters and returns it. The
// counters are the base of the money (see "The Oracle on DeepSeek" in the README); the
// guard prices the same numbers for its daily limit.
func countPaidTokens(provider string, reply []byte) (hit, miss, out float64) {
	hit, miss, out = lastNumber(usageHit, reply), lastNumber(usageMiss, reply), lastNumber(usageOutput, reply)
	for kind, n := range map[string]float64{"cache_hit": hit, "cache_miss": miss, "output": out} {
		if n > 0 {
			metrics.PaidLLMTokens.WithLabelValues(provider, kind).Add(n)
		}
	}
	return hit, miss, out
}
