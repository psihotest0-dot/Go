package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const okCompletion = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`

const contextExceededBody = `{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}}`

const okStream = "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"

// newTestClient returns a client wired to a fake OpenAI compatible server.
func newTestClient(t *testing.T, opts Options, handler http.HandlerFunc) *LemonadeClient {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	opts.BaseURL = srv.URL + "/v1"
	c := NewLemonadeClientWithOptions(opts)
	c.retryBaseDelay = time.Millisecond
	return c
}

func userRequest(content string) openai.ChatCompletionRequest {
	return openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: content}},
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestChatDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusBadRequest, contextExceededBody)
	})

	_, err := c.Chat(context.Background(), userRequest("hi"))

	require.Error(t, err)
	assert.EqualValues(t, 1, calls.Load(), "a 400 is the same answer every time")
	var apiErr *openai.APIError
	require.True(t, errors.As(err, &apiErr), "the cause stays inspectable through the wrapping")
	assert.Equal(t, http.StatusBadRequest, apiErr.HTTPStatusCode)
	assert.Contains(t, err.Error(), "context size")
}

func TestChatRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			writeJSON(w, http.StatusServiceUnavailable, `{"error":{"message":"loading model"}}`)
			return
		}
		writeJSON(w, http.StatusOK, okCompletion)
	})

	resp, err := c.Chat(context.Background(), userRequest("hi"))

	require.NoError(t, err)
	assert.EqualValues(t, 3, calls.Load())
	assert.Equal(t, "ok", resp.Choices[0].Message.Content)
}

func TestChatRetriesRateLimits(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(w, http.StatusTooManyRequests, `{"error":{"message":"busy"}}`)
			return
		}
		writeJSON(w, http.StatusOK, okCompletion)
	})

	_, err := c.Chat(context.Background(), userRequest("hi"))

	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
}

func TestChatGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	})

	_, err := c.Chat(context.Background(), userRequest("hi"))

	require.Error(t, err)
	assert.EqualValues(t, maxAttempts, calls.Load())
	assert.Contains(t, err.Error(), fmt.Sprintf("failed after %d attempts", maxAttempts))
	var apiErr *openai.APIError
	assert.True(t, errors.As(err, &apiErr), "the last failure is wrapped, not flattened to text")
}

func TestChatStopsRetryingWhenContextEnds(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	})
	c.retryBaseDelay = time.Minute // would never finish on its own

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		eventually(t, "first attempt", func() bool { return calls.Load() >= 1 })
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, userRequest("hi"))
		done <- err
	}()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.EqualValues(t, 1, calls.Load())
	case <-time.After(3 * time.Second):
		t.Fatal("Chat kept waiting after its context was cancelled")
	}
}

func TestChatWithJSONSchemaDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusBadRequest, contextExceededBody)
	})

	_, err := c.ChatWithJSONSchema(context.Background(), userRequest("hi"), []string{})

	require.Error(t, err)
	assert.EqualValues(t, 1, calls.Load())
	var statusErr *StatusError
	require.True(t, errors.As(err, &statusErr))
	assert.Equal(t, http.StatusBadRequest, statusErr.StatusCode)
	assert.Contains(t, statusErr.Body, "context size")
}

func TestChatWithJSONSchemaRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	var body map[string]any
	c := newTestClient(t, Options{Model: "pinned"}, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(w, http.StatusBadGateway, `bad gateway`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeJSON(w, http.StatusOK, okCompletion)
	})

	resp, err := c.ChatWithJSONSchema(context.Background(), userRequest("hi"), []string{})

	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
	assert.Equal(t, "ok", resp.Choices[0].Message.Content)
	assert.Equal(t, "pinned", body["model"])
	assert.Contains(t, body, "response_format")
}

func TestStatusErrorBodyIsBounded(t *testing.T) {
	c := newTestClient(t, Options{Model: "m"}, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, strings.Repeat("x", 100_000))
	})

	_, err := c.ChatWithJSONSchema(context.Background(), userRequest("hi"), []string{})

	var statusErr *StatusError
	require.True(t, errors.As(err, &statusErr))
	assert.LessOrEqual(t, len(statusErr.Body), maxErrorBodyBytes)
}

// modelServer answers /v1/models with the given status and ids, and chat
// completions with okCompletion while recording the model it was asked for.
type modelServer struct {
	modelCalls atomic.Int32
	mu         sync.Mutex
	chatModels []string
}

func (s *modelServer) handler(status int, ids ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			s.modelCalls.Add(1)
			if status != http.StatusOK {
				writeJSON(w, status, `{"error":{"message":"nope"}}`)
				return
			}
			data := make([]string, 0, len(ids))
			for _, id := range ids {
				data = append(data, fmt.Sprintf(`{"id":%q,"object":"model"}`, id))
			}
			writeJSON(w, http.StatusOK, `{"object":"list","data":[`+strings.Join(data, ",")+`]}`)
			return
		}

		var req openai.ChatCompletionRequest
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		s.mu.Lock()
		s.chatModels = append(s.chatModels, req.Model)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, okCompletion)
	}
}

func (s *modelServer) models() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.chatModels...)
}

func TestPinnedModelSkipsDiscovery(t *testing.T) {
	srv := &modelServer{}
	c := newTestClient(t, Options{Model: "Qwen3.5-9B"}, srv.handler(http.StatusOK, "something-else"))

	_, err := c.Chat(context.Background(), userRequest("hi"))
	require.NoError(t, err)

	assert.Equal(t, []string{"Qwen3.5-9B"}, srv.models())
	assert.Zero(t, srv.modelCalls.Load(), "LLM_MODEL means no lookup")
}

func TestExplicitRequestModelIsKeptUnlessForced(t *testing.T) {
	srv := &modelServer{}
	c := newTestClient(t, Options{Model: "pinned"}, srv.handler(http.StatusOK))

	req := userRequest("hi")
	req.Model = "custom"
	_, err := c.Chat(context.Background(), req)
	require.NoError(t, err)

	forced := newTestClient(t, Options{Model: "pinned", ForceModel: true}, srv.handler(http.StatusOK))
	_, err = forced.Chat(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, []string{"custom", "pinned"}, srv.models())
}

func TestDiscoveredModelIsChosenByNameAndCached(t *testing.T) {
	srv := &modelServer{}
	// The server's order must not decide which model is used.
	c := newTestClient(t, Options{}, srv.handler(http.StatusOK, "zeta", "alpha", "mid"))

	for i := 0; i < 3; i++ {
		_, err := c.Chat(context.Background(), userRequest("hi"))
		require.NoError(t, err)
	}

	assert.Equal(t, []string{"alpha", "alpha", "alpha"}, srv.models())
	assert.EqualValues(t, 1, srv.modelCalls.Load())
}

func TestFailedModelDiscoveryIsCached(t *testing.T) {
	srv := &modelServer{}
	c := newTestClient(t, Options{}, srv.handler(http.StatusInternalServerError))

	for i := 0; i < 3; i++ {
		_, err := c.Chat(context.Background(), userRequest("hi"))
		require.NoError(t, err)
	}

	assert.Equal(t, []string{fallbackModel, fallbackModel, fallbackModel}, srv.models())
	assert.EqualValues(t, 1, srv.modelCalls.Load(), "an unhealthy /models must not be asked for every request")
}

func TestCancelledDiscoveryIsNotCached(t *testing.T) {
	srv := &modelServer{}
	c := newTestClient(t, Options{}, srv.handler(http.StatusOK, "alpha"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, fallbackModel, c.resolveModel(ctx))

	assert.Equal(t, "alpha", c.resolveModel(context.Background()),
		"the caller giving up says nothing about the server")
}

// TestBackgroundRequestsRunOneAtATime is the scenario the limiter exists for:
// three background jobs fire at once, the server must see them one by one, and a
// chat request still gets through while one of them is running.
func TestBackgroundRequestsRunOneAtATime(t *testing.T) {
	var inFlight, peak atomic.Int32
	gate := make(chan struct{})
	var openOnce sync.Once
	openGate := func() { openOnce.Do(func() { close(gate) }) }

	c := newTestClient(t, Options{Model: "m", Slots: 2, BackgroundSlots: 1}, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "background") {
			writeJSON(w, http.StatusOK, okCompletion)
			return
		}

		now := inFlight.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		<-gate
		inFlight.Add(-1)
		writeJSON(w, http.StatusOK, okCompletion)
	})
	// Registered after the server's own cleanup, so it runs first: a failed
	// assertion must not leave handlers blocked (Close waits for them) or
	// goroutines reporting into a finished test.
	var wg sync.WaitGroup
	t.Cleanup(func() {
		openGate()
		wg.Wait()
	})

	bgCtx := WithBackground(context.Background())
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Chat(bgCtx, userRequest("background"))
			assert.NoError(t, err)
		}()
	}

	eventually(t, "a background request reaches the server", func() bool { return inFlight.Load() == 1 })
	eventually(t, "the other two wait for a slot", func() bool {
		_, _, _, waiting := c.limiter.snapshot()
		return waiting == 2
	})
	assert.EqualValues(t, 1, inFlight.Load())

	chatDone := make(chan error, 1)
	go func() {
		_, err := c.Chat(context.Background(), userRequest("chat"))
		chatDone <- err
	}()
	select {
	case err := <-chatDone:
		assert.NoError(t, err, "a chat request must not queue behind background work")
	case <-time.After(3 * time.Second):
		t.Fatal("chat request waited for background work")
	}

	openGate()
	wg.Wait()
	assert.EqualValues(t, 1, peak.Load(), "background requests never overlapped")
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, okStream)
}

func TestChatStreamKeepsSlotUntilClosed(t *testing.T) {
	c := newTestClient(t, Options{Model: "m", Slots: 1, BackgroundSlots: 1}, streamHandler)

	first, err := c.ChatStream(context.Background(), userRequest("one"))
	require.NoError(t, err)

	secondDone := make(chan Stream, 1)
	go func() {
		s, err := c.ChatStream(context.Background(), userRequest("two"))
		assert.NoError(t, err)
		secondDone <- s
	}()
	eventually(t, "second stream queued", func() bool {
		_, _, waiting, _ := c.limiter.snapshot()
		return waiting == 1
	})

	select {
	case <-secondDone:
		t.Fatal("second stream started while the first one still holds the only slot")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, first.Close())
	require.NoError(t, first.Close(), "closing twice is harmless")

	select {
	case second := <-secondDone:
		require.NotNil(t, second)
		require.NoError(t, second.Close())
	case <-time.After(3 * time.Second):
		t.Fatal("second stream never got the slot")
	}

	active, _, _, _ := c.limiter.snapshot()
	assert.Zero(t, active, "a double Close must not free more than one slot")
}

func TestChatStreamFreesSlotAtEndOfStream(t *testing.T) {
	c := newTestClient(t, Options{Model: "m", Slots: 1, BackgroundSlots: 1}, streamHandler)

	stream, err := c.ChatStream(context.Background(), userRequest("one"))
	require.NoError(t, err)
	defer stream.Close()

	chunk, err := stream.Recv()
	require.NoError(t, err)
	assert.Equal(t, "hi", chunk.Choices[0].Delta.Content)

	active, _, _, _ := c.limiter.snapshot()
	assert.Equal(t, 1, active, "the slot is busy while the model is still answering")

	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)

	active, _, _, _ = c.limiter.snapshot()
	assert.Zero(t, active, "the model is done, even if the caller has not closed the stream yet")
}

func TestChatStreamFailureFreesSlotAndReturnsNilStream(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, Options{Model: "m", Slots: 1, BackgroundSlots: 1}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusBadRequest, contextExceededBody)
	})

	stream, err := c.ChatStream(context.Background(), userRequest("hi"))

	require.Error(t, err)
	assert.True(t, stream == nil, "a failed call must give a nil interface, not a typed nil")
	assert.EqualValues(t, 1, calls.Load(), "a 400 is not repeated")
	active, _, _, _ := c.limiter.snapshot()
	assert.Zero(t, active)
}

func TestChatStreamReturnsContextErrorWhileWaitingForSlot(t *testing.T) {
	c := newTestClient(t, Options{Model: "m", Slots: 1, BackgroundSlots: 1}, streamHandler)

	holder, err := c.ChatStream(context.Background(), userRequest("holder"))
	require.NoError(t, err)
	defer holder.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	stream, err := c.ChatStream(ctx, userRequest("waiter"))

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, stream == nil)
}

func TestMockLLMStreamErrorGivesNilStream(t *testing.T) {
	mock := &MockLLM{Error: errors.New("down")}

	stream, err := mock.ChatStream(context.Background(), userRequest("hi"))

	require.Error(t, err)
	assert.True(t, stream == nil)
}
