package oracle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
)

const testToken = "0123456789abcdef0123456789abcdef0123" // 36 characters

type routes struct {
	mux   *http.ServeMux
	model *fakeLLM
	store *memStore
}

// newRoutes is the Oracle on a mux the way main.go builds it: the bot routes on the
// outer mux, the journal on a second one.
func newRoutes(t *testing.T, replies ...reply) routes {
	t.Helper()
	model := &fakeLLM{replies: replies}
	svc, store, _ := newTestService(model)
	h := NewHandler(svc, store, testToken)
	mux := http.NewServeMux()
	h.RegisterBotRoutes(mux)
	h.RegisterReadRoutes(mux)
	return routes{mux: mux, model: model, store: store}
}

func (r routes) do(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.RequestIDKey, "req-from-middleware"))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

func TestEvaluateAnswersTheBot(t *testing.T) {
	r := newRoutes(t, good(VerdictApprove, "Трендът е нагоре."))

	rec := r.do(http.MethodPost, "/oracle/evaluate", marshal(t, validRequest()), bearer(testToken))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := decodeBody(t, rec)
	assert.Equal(t, "APPROVE", body["verdict"])
	assert.Equal(t, "Трендът е нагоре.", body["reason"])
	assert.Equal(t, "req-from-middleware", body["request_id"], "the id the middleware gave the call")
	assert.Equal(t, "test-model", body["model"])
	assert.Equal(t, false, body["cached"])
	assert.Contains(t, body, "latency_ms")
}

func TestTheRouteRefusesACallWithoutTheRightToken(t *testing.T) {
	short := testToken[:len(testToken)-1]
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"no header", nil},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}},
		{"the token without the scheme", map[string]string{"Authorization": testToken}},
		{"another scheme", map[string]string{"Authorization": "Basic " + testToken}},
		{"a lower case scheme", map[string]string{"Authorization": "bearer " + testToken}},
		{"a wrong token of the same length", bearer(strings.Repeat("x", len(testToken)))},
		{"the token minus a character", bearer(short)},
		{"the token plus a character", bearer(testToken + "0")},
		{"the token in another header", map[string]string{"X-Oracle-Token": testToken}},
		{"the user header of the chat", map[string]string{"X-User-ID": "123e4567-e89b-12d3-a456-426614174000"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRoutes(t, good(VerdictApprove, "x"))
			before := requestsWith(t, "unauthorized")

			rec := r.do(http.MethodPost, "/oracle/evaluate", marshal(t, validRequest()), tt.headers)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, `Bearer realm="oracle"`, rec.Header().Get("WWW-Authenticate"))
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
			assert.Equal(t, 0, r.model.callCount(), "the model is never reached without the token")
			assert.Empty(t, r.store.snapshot())
			assert.Equal(t, before+1, requestsWith(t, "unauthorized"))
		})
	}
}

func TestTheTokenCheckComesBeforeAnythingElse(t *testing.T) {
	// A bad body and a bad method with a wrong token are 401, not 400 or 405: the route
	// tells a stranger nothing about itself.
	r := newRoutes(t)
	assert.Equal(t, http.StatusUnauthorized, r.do(http.MethodPost, "/oracle/evaluate", []byte("garbage"), nil).Code)
	assert.Equal(t, http.StatusUnauthorized, r.do(http.MethodGet, "/oracle/evaluate", nil, nil).Code)
	assert.Equal(t, http.StatusUnauthorized, r.do(http.MethodGet, "/oracle/ping", nil, nil).Code)
}

func TestTheTokenIsNeverLogged(t *testing.T) {
	// The failed-token log line has the path and the remote address, not what was sent.
	r := newRoutes(t)
	rec := r.do(http.MethodPost, "/oracle/evaluate", marshal(t, validRequest()), bearer("SECRET-GUESS-NOT-THE-TOKEN-0123456789"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), "SECRET-GUESS")
}

func TestEvaluateOnlyTakesPost(t *testing.T) {
	r := newRoutes(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := r.do(method, "/oracle/evaluate", nil, bearer(testToken))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
	}
}

func TestEvaluateRefusesABadBody(t *testing.T) {
	r := newRoutes(t, good(VerdictApprove, "x"))
	before := requestsWith(t, "bad_request")

	rec := r.do(http.MethodPost, "/oracle/evaluate", marshal(t, validRequest(func(b map[string]any) {
		b["signal"].(map[string]any)["symbol"] = "not a symbol — SECRET-IN-BODY"
	})), bearer(testToken))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	body := decodeBody(t, rec)
	assert.Equal(t, "invalid_request", body["error"])
	assert.Contains(t, body["detail"], "signal.symbol")
	assert.NotContains(t, rec.Body.String(), "SECRET-IN-BODY", "the answer names the field, not what was in it")
	assert.Equal(t, 0, r.model.callCount())
	assert.Equal(t, before+1, requestsWith(t, "bad_request"))

	rec = r.do(http.MethodPost, "/oracle/evaluate", []byte("not json"), bearer(testToken))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = r.do(http.MethodPost, "/oracle/evaluate", nil, bearer(testToken))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEvaluateRefusesABodyOverTheLimit(t *testing.T) {
	r := newRoutes(t, good(VerdictApprove, "x"))
	big := marshal(t, validRequest(func(b map[string]any) { b["rationale"] = strings.Repeat("x", MaxBodyBytes) }))
	require.Greater(t, len(big), MaxBodyBytes)

	rec := r.do(http.MethodPost, "/oracle/evaluate", big, bearer(testToken))

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.JSONEq(t, `{"error":"body_too_large"}`, rec.Body.String())
	assert.Equal(t, 0, r.model.callCount())
}

func TestEvaluateAnswersFailuresWithAnErrorTheBotCanTellApart(t *testing.T) {
	tests := []struct {
		name    string
		replies []reply
		status  int
		code    string
	}{
		{"the model is down", []reply{{err: errors.New("connection refused")}}, http.StatusBadGateway, "model_unavailable"},
		{"the model times out", []reply{{err: context.DeadlineExceeded}}, http.StatusGatewayTimeout, "model_timeout"},
		{"the model answers nonsense twice", []reply{loop(), loop()}, http.StatusBadGateway, "unusable_reply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRoutes(t, tt.replies...)
			before := requestsWith(t, "failed")

			rec := r.do(http.MethodPost, "/oracle/evaluate", marshal(t, validRequest()), bearer(testToken))

			assert.Equal(t, tt.status, rec.Code)
			assert.JSONEq(t, `{"error":"`+tt.code+`"}`, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "connection refused", "the cause stays in the log")
			assert.Equal(t, before+1, requestsWith(t, "failed"))
			rows := r.store.snapshot()
			require.Len(t, rows, 1, "a failure is journaled too")
			assert.Equal(t, StatusFailed, rows[0].Status)
		})
	}
}

func TestEvaluateNeverReturnsAVerdictTheModelDidNotGive(t *testing.T) {
	// Whatever fails, the body has no "verdict": the bot must read a failure as a HOLD
	// of its own, and must not find a word to act on.
	r := newRoutes(t, loop(), loop())
	rec := r.do(http.MethodPost, "/oracle/evaluate", marshal(t, validRequest()), bearer(testToken))
	assert.NotContains(t, decodeBody(t, rec), "verdict")
}

func TestPing(t *testing.T) {
	r := newRoutes(t)

	rec := r.do(http.MethodGet, "/oracle/ping", nil, bearer(testToken))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
	assert.Equal(t, 0, r.model.callCount(), "a ping does not cost a model call")

	rec = r.do(http.MethodPost, "/oracle/ping", nil, bearer(testToken))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestTokenUsable(t *testing.T) {
	assert.False(t, TokenUsable(""))
	assert.False(t, TokenUsable(strings.Repeat("a", MinTokenLength-1)))
	assert.True(t, TokenUsable(strings.Repeat("a", MinTokenLength)))
	assert.True(t, TokenUsable(testToken))
}

func seed(t *testing.T, r routes, symbols ...string) {
	t.Helper()
	for i, s := range symbols {
		_, err := r.store.Save(context.Background(), Evaluation{
			SignalID: "sig-" + string(rune('a'+i)), Symbol: s, Status: StatusAnswered, Verdict: VerdictHold, Reason: "r",
		})
		require.NoError(t, err)
	}
}

func TestDecisionsListsTheJournalNewestFirst(t *testing.T) {
	r := newRoutes(t)
	seed(t, r, "AAPL", "MSFT", "AAPL")

	rec := r.do(http.MethodGet, "/oracle/decisions", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Decisions    []Evaluation `json:"decisions"`
		NextBeforeID int64        `json:"next_before_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Decisions, 3)
	assert.Equal(t, int64(3), body.Decisions[0].ID)
	assert.Equal(t, int64(1), body.Decisions[2].ID)
	assert.Zero(t, body.NextBeforeID, "the only page")
	assert.NotContains(t, rec.Body.String(), `"request"`, "the request bodies are not in the list")
}

func TestDecisionsPagesAndFilters(t *testing.T) {
	r := newRoutes(t)
	seed(t, r, "AAPL", "MSFT", "AAPL", "MSFT", "AAPL")

	rec := r.do(http.MethodGet, "/oracle/decisions?limit=2", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var page struct {
		Decisions    []Evaluation `json:"decisions"`
		NextBeforeID int64        `json:"next_before_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Decisions, 2)
	assert.Equal(t, int64(4), page.NextBeforeID, "the id of the last row is the cursor of the next page")

	rec = r.do(http.MethodGet, "/oracle/decisions?limit=2&before_id=4", nil, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Decisions, 2)
	assert.Equal(t, int64(3), page.Decisions[0].ID)

	rec = r.do(http.MethodGet, "/oracle/decisions?symbol=msft", nil, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Decisions, 2)
	for _, d := range page.Decisions {
		assert.Equal(t, "MSFT", d.Symbol)
	}
}

func TestDecisionsRefusesBadParameters(t *testing.T) {
	r := newRoutes(t)
	for _, query := range []string{"limit=0", "limit=201", "limit=abc", "before_id=-1", "before_id=x", "symbol=A%20B"} {
		rec := r.do(http.MethodGet, "/oracle/decisions?"+query, nil, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, query)
	}
	assert.Equal(t, http.StatusMethodNotAllowed, r.do(http.MethodPost, "/oracle/decisions", nil, nil).Code)
}

func TestTheJournalIsReadWithoutTheBotToken(t *testing.T) {
	// It is behind the user check of the chat API, not behind the bot's token, and the
	// bot's token is not what opens it. (main.go puts it on the user-protected mux.)
	r := newRoutes(t)
	rec := r.do(http.MethodGet, "/oracle/decisions", nil, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestEvaluateReplyCarriesThePromptVersion(t *testing.T) {
	r := newRoutes(t, good(VerdictApprove, "Трендът е нагоре."))

	rec := r.do(http.MethodPost, "/oracle/evaluate", marshal(t, validRequest()), bearer(testToken))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeBody(t, rec)
	assert.Equal(t, currentPromptParts("SYSTEM PROMPT").version(), body["prompt_version"])
	// The six fields of the contract are still all there, so a bot that knows only them keeps working.
	for _, field := range []string{"verdict", "reason", "request_id", "model", "cached", "latency_ms"} {
		assert.Contains(t, body, field)
	}
}

func TestDecisionsShowTheVersionsWhenThereAreAnyAndOmitThemWhenNot(t *testing.T) {
	r := newRoutes(t)
	_, err := r.store.Save(context.Background(), Evaluation{
		SignalID: "sig-1", Symbol: "AAPL", Status: StatusAnswered, Verdict: VerdictVeto, Reason: "r",
		PromptVersion: "0123456789ab", SignalVersion: "oracle-probe",
	})
	require.NoError(t, err)
	_, err = r.store.Save(context.Background(), Evaluation{
		SignalID: "sig-2", Symbol: "KO", Status: StatusAnswered, Verdict: VerdictHold, Reason: "r",
	})
	require.NoError(t, err)

	rec := r.do(http.MethodGet, "/oracle/decisions", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Decisions []map[string]any `json:"decisions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Decisions, 2)
	newest, oldest := body.Decisions[0], body.Decisions[1]
	assert.NotContains(t, newest, "prompt_version", "omitted when empty, so old clients see no change")
	assert.NotContains(t, newest, "signal_version")
	assert.Equal(t, "0123456789ab", oldest["prompt_version"])
	assert.Equal(t, "oracle-probe", oldest["signal_version"])
}
