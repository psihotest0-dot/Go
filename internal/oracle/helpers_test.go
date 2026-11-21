package oracle

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// reply is one scripted answer of the fake model.
type reply struct {
	content string
	finish  openai.FinishReason
	err     error
	model   string
	usage   openai.Usage
}

func good(verdict, why string) reply {
	body, _ := json.Marshal(Answer{Verdict: verdict, Reason: why})
	return reply{
		content: string(body), finish: openai.FinishReasonStop, model: "test-model",
		usage: openai.Usage{PromptTokens: 700, CompletionTokens: 30},
	}
}

// loop is what a model that ran into a repetition loop leaves: JSON cut off at the
// token limit.
func loop() reply {
	return reply{
		content: `{"verdict": "HOLD", "reason": "Volume is low and-and-and-and-and-and-and-and`,
		finish:  openai.FinishReasonLength, model: "test-model",
		usage: openai.Usage{PromptTokens: 700, CompletionTokens: 200},
	}
}

// fakeLLM answers ChatWithJSONSchema from a script and remembers what it was asked.
type fakeLLM struct {
	mu      sync.Mutex
	replies []reply
	calls   int
	last    openai.ChatCompletionRequest
	schema  any
	// gate, when set, holds every call until it is closed; started is signalled when
	// a call has begun.
	gate    chan struct{}
	started chan struct{}
}

func (f *fakeLLM) Chat(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	panic("the Oracle must not use Chat")
}

func (f *fakeLLM) ChatStream(context.Context, openai.ChatCompletionRequest) (llm.Stream, error) {
	panic("the Oracle must not use ChatStream")
}

func (f *fakeLLM) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.last = req
	f.schema = schema
	gate, started := f.gate, f.started
	f.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return openai.ChatCompletionResponse{}, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if n > len(f.replies) {
		return openai.ChatCompletionResponse{}, nil // no choices at all
	}
	r := f.replies[n-1]
	if r.err != nil {
		return openai.ChatCompletionResponse{}, r.err
	}
	return openai.ChatCompletionResponse{
		Model: r.model,
		Choices: []openai.ChatCompletionChoice{{
			Message:      openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: r.content},
			FinishReason: r.finish,
		}},
		Usage: r.usage,
	}, nil
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// memStore is a journal in memory.
type memStore struct {
	mu       sync.Mutex
	rows     []Evaluation
	failSave bool
}

func (m *memStore) Save(_ context.Context, e Evaluation) (Evaluation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave {
		return e, context.DeadlineExceeded
	}
	e.ID = int64(len(m.rows) + 1)
	e.CreatedAt = time.Now()
	m.rows = append(m.rows, e)
	return e, nil
}

// SaveServed does what the database does: a row only when the signal has none in the mode.
func (m *memStore) SaveServed(ctx context.Context, e Evaluation) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave {
		return false, context.DeadlineExceeded
	}
	for _, r := range m.rows {
		if r.SignalID == e.SignalID && r.Mode == e.Mode {
			return false, nil
		}
	}
	e.ID = int64(len(m.rows) + 1)
	e.CreatedAt = time.Now()
	m.rows = append(m.rows, e)
	return true, nil
}

func (m *memStore) List(_ context.Context, f ListFilter) ([]Evaluation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Evaluation
	for i := len(m.rows) - 1; i >= 0 && len(out) < f.Limit; i-- {
		r := m.rows[i]
		if f.Symbol != "" && r.Symbol != f.Symbol {
			continue
		}
		if f.BeforeID != 0 && r.ID >= f.BeforeID {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) snapshot() []Evaluation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Evaluation(nil), m.rows...)
}

// clock is a time the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 5, 15, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestService is a Service on a fake model, an in-memory journal and a clock of
// its own, with a fixed system prompt.
func newTestService(f *fakeLLM) (*Service, *memStore, *clock) {
	store := &memStore{}
	clk := newClock()
	svc := NewService(f, store)
	svc.prompt = func() (string, error) { return "SYSTEM PROMPT", nil }
	svc.now = clk.Now
	return svc, store, clk
}

// validRequest is a body the Oracle accepts, as the bot would send it.
func validRequest(mutate ...func(map[string]any)) map[string]any {
	body := map[string]any{
		"schema_version": 1,
		"mode":           "shadow",
		"signal": map[string]any{
			"signal_id":            "sig-0001",
			"symbol":               "AAPL",
			"direction":            "LONG",
			"timeframe":            "15Min",
			"version":              "trend_cross_v1",
			"closed_bar_timestamp": "2026-11-05T15:15:00+00:00",
			"expires_at":           "2026-11-05T15:45:00+00:00",
			"confidence":           1.0,
		},
		"rationale":    "15-minute EMA cross up with RSI in range and the hourly trend up.",
		"technicals":   map[string]any{"close": 123.4, "rsi": 55.5, "hourly_trend_up": true},
		"recent_bars":  []any{map[string]any{"start": "2026-11-05T14:45:00+00:00", "close": 123.1, "volume": 1000}},
		"volume_ratio": 1.2,
		"live_price":   "123.50",
		"context":      map[string]any{"equity": "102382.10", "open_positions": 1},
		"news": []any{
			map[string]any{"article_id": 7, "published_at": "2026-11-05T12:00:00+00:00", "source": "benzinga",
				"headline": "Apple raises its dividend", "summary": "The board approved a 4% increase.", "other_symbols": 0},
		},
	}
	for _, m := range mutate {
		m(body)
	}
	return body
}

func marshal(t testing.TB, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

// mustDecode is a valid request, decoded.
func mustDecode(t testing.TB, mutate ...func(map[string]any)) (*Request, []byte) {
	t.Helper()
	raw := marshal(t, validRequest(mutate...))
	req, err := DecodeBytes(raw)
	require.NoError(t, err)
	return req, raw
}

func reflectTypeField(v any, name string) (reflect.StructField, bool) {
	return reflect.TypeOf(v).FieldByName(name)
}

// requestsWith is how many calls the route has counted with the given outcome, in every mode.
func requestsWith(t testing.TB, outcome string) float64 {
	t.Helper()
	return oracleCounter(t, "oracle_requests_total", map[string]string{"outcome": outcome})
}

// requestsIn is the same for one mode ("shadow", "active" or "none").
func requestsIn(t testing.TB, outcome, mode string) float64 {
	t.Helper()
	return oracleCounter(t, "oracle_requests_total", map[string]string{"outcome": outcome, "mode": mode})
}

// verdictsIn is how many verdicts the model has given in the mode.
func verdictsIn(t testing.TB, verdict, mode string) float64 {
	t.Helper()
	return oracleCounter(t, "oracle_verdicts_total", map[string]string{"verdict": verdict, "mode": mode})
}

// latencyCountIn is how many calls that reached the model the latency histogram has seen in the mode.
func latencyCountIn(t testing.TB, mode string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "oracle_latency_seconds" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "mode" && l.GetValue() == mode {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// oracleCounter adds up the series of the counter that carry every one of the labels.
func oracleCounter(t testing.TB, name string, want map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	var total float64
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			have := map[string]string{}
			for _, l := range m.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			match := true
			for k, v := range want {
				if have[k] != v {
					match = false
				}
			}
			if match {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

// waitServed waits for the light journal rows that are being written in the background.
func waitServed(svc *Service) { svc.served.Wait() }
