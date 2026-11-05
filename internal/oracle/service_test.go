package oracle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluateAsksTheModelAndJournalsTheAnswer(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictVeto, "Обемът е малък и новината е лоша.")}}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	res, err := svc.Evaluate(context.Background(), req, raw, "req-1")

	require.NoError(t, err)
	assert.Equal(t, VerdictVeto, res.Verdict)
	assert.Equal(t, "Обемът е малък и новината е лоша.", res.Reason)
	assert.Equal(t, "req-1", res.RequestID)
	assert.Equal(t, "test-model", res.Model)
	assert.False(t, res.Cached)

	rows := store.snapshot()
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, StatusAnswered, row.Status)
	assert.Equal(t, VerdictVeto, row.Verdict)
	assert.Equal(t, "sig-0001", row.SignalID)
	assert.Equal(t, "AAPL", row.Symbol)
	assert.Equal(t, ModeShadow, row.Mode)
	assert.Equal(t, "req-1", row.RequestID)
	assert.Equal(t, 1, row.Attempts)
	assert.Equal(t, 700, row.PromptTokens)
	assert.Equal(t, 30, row.CompletionTokens)
	assert.Equal(t, "stop", row.FinishReason)
	assert.JSONEq(t, string(raw), string(row.Request), "the body is journaled as received")
}

// The model gets the system prompt and the facts, and nothing else: no tools, no
// history, no memory. This is what bounds a planted headline.
func TestTheModelGetsOnlyThePromptAndTheFacts(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictHold, "x")}}
	svc, _, _ := newTestService(model)
	req, raw := mustDecode(t)

	_, err := svc.Evaluate(context.Background(), req, raw, "r")
	require.NoError(t, err)

	last := model.last
	require.Len(t, last.Messages, 2)
	assert.Equal(t, openai.ChatMessageRoleSystem, last.Messages[0].Role)
	assert.Equal(t, "SYSTEM PROMPT", last.Messages[0].Content)
	assert.Equal(t, openai.ChatMessageRoleUser, last.Messages[1].Role)
	assert.Contains(t, last.Messages[1].Content, "symbol: AAPL")
	assert.Empty(t, last.Tools, "the model has no tools")
	assert.Empty(t, last.Functions)
	assert.Nil(t, last.ToolChoice)
	assert.Equal(t, maxAnswerTokens, last.MaxTokens)
	assert.InDelta(t, temperature, last.Temperature, 1e-6)
	assert.IsType(t, Answer{}, model.schema, "the reply is held to the Answer schema")
}

func TestTheAnswerSchemaAllowsOnlyTheThreeWords(t *testing.T) {
	// The enum tag is what the model server turns into a grammar.
	field, ok := reflectTypeField(Answer{}, "Verdict")
	require.True(t, ok)
	assert.Equal(t, "APPROVE,VETO,HOLD", field.Tag.Get("enum"))
}

func TestEvaluateRepeatsFromMemory(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "Трендът е нагоре.")}}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	first, err := svc.Evaluate(context.Background(), req, raw, "req-1")
	require.NoError(t, err)
	second, err := svc.Evaluate(context.Background(), req, raw, "req-2")
	require.NoError(t, err)

	assert.Equal(t, 1, model.callCount(), "the same signal is not put to the model twice")
	assert.False(t, first.Cached)
	assert.True(t, second.Cached)
	assert.Equal(t, first.Verdict, second.Verdict)
	assert.Equal(t, first.Reason, second.Reason)
	assert.Equal(t, "req-2", second.RequestID, "the repeat carries the id of the new call")
	assert.Len(t, store.snapshot(), 1, "a repeat is not a new journal row")
}

func TestAHoldIsRepeatedForAFewMinutesAndThenAskedAgain(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictHold, "Чака се обем."), good(VerdictApprove, "Обемът дойде.")}}
	svc, _, clk := newTestService(model)
	req, raw := mustDecode(t)

	r1, err := svc.Evaluate(context.Background(), req, raw, "1")
	require.NoError(t, err)
	assert.Equal(t, VerdictHold, r1.Verdict)

	clk.Advance(holdCacheTTL - time.Second)
	r2, err := svc.Evaluate(context.Background(), req, raw, "2")
	require.NoError(t, err)
	assert.True(t, r2.Cached, "inside the window the HOLD is repeated")
	assert.Equal(t, 1, model.callCount())

	clk.Advance(2 * time.Second)
	r3, err := svc.Evaluate(context.Background(), req, raw, "3")
	require.NoError(t, err)
	assert.False(t, r3.Cached, "after the window the question goes to the model again")
	assert.Equal(t, VerdictApprove, r3.Verdict)
	assert.Equal(t, 2, model.callCount())
}

func TestAFinalVerdictIsKeptLongerThanAHold(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictVeto, "Не."), good(VerdictApprove, "Да.")}}
	svc, _, clk := newTestService(model)
	req, raw := mustDecode(t)

	_, err := svc.Evaluate(context.Background(), req, raw, "1")
	require.NoError(t, err)

	clk.Advance(holdCacheTTL + time.Minute) // a HOLD would have lapsed by now
	r, err := svc.Evaluate(context.Background(), req, raw, "2")
	require.NoError(t, err)
	assert.True(t, r.Cached)
	assert.Equal(t, VerdictVeto, r.Verdict)

	clk.Advance(finalCacheTTL)
	r, err = svc.Evaluate(context.Background(), req, raw, "3")
	require.NoError(t, err)
	assert.False(t, r.Cached)
	assert.Equal(t, VerdictApprove, r.Verdict)
}

func TestDifferentSignalsAreSeparate(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "a"), good(VerdictVeto, "b")}}
	svc, _, _ := newTestService(model)
	one, rawOne := mustDecode(t)
	two, rawTwo := mustDecode(t, func(b map[string]any) { b["signal"].(map[string]any)["signal_id"] = "sig-0002" })

	r1, err := svc.Evaluate(context.Background(), one, rawOne, "1")
	require.NoError(t, err)
	r2, err := svc.Evaluate(context.Background(), two, rawTwo, "2")
	require.NoError(t, err)

	assert.Equal(t, VerdictApprove, r1.Verdict)
	assert.Equal(t, VerdictVeto, r2.Verdict)
	assert.Equal(t, 2, model.callCount())
}

// The case of 2026-11-05: a reply that loops until the token limit leaves unfinished
// JSON. Sent again, the same question comes back normal.
func TestALoopedReplyIsAskedAgain(t *testing.T) {
	model := &fakeLLM{replies: []reply{loop(), good(VerdictHold, "Обемът е малък.")}}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	res, err := svc.Evaluate(context.Background(), req, raw, "r")

	require.NoError(t, err)
	assert.Equal(t, VerdictHold, res.Verdict)
	assert.Equal(t, 2, model.callCount())
	row := store.snapshot()[0]
	assert.Equal(t, StatusAnswered, row.Status)
	assert.Equal(t, 2, row.Attempts)
	assert.Equal(t, 1400, row.PromptTokens, "the tokens of both calls are counted")
	assert.Equal(t, 230, row.CompletionTokens)
	assert.Equal(t, "stop", row.FinishReason, "the finish reason is the one of the reply that was used")
}

func TestTwoUnusableRepliesAreAFailureAndAreNotCached(t *testing.T) {
	model := &fakeLLM{replies: []reply{loop(), loop(), good(VerdictApprove, "Добре.")}}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	_, err := svc.Evaluate(context.Background(), req, raw, "1")

	require.ErrorIs(t, err, ErrUnusableReply)
	assert.Equal(t, maxAttempts, model.callCount())
	row := store.snapshot()[0]
	assert.Equal(t, StatusFailed, row.Status)
	assert.Empty(t, row.Verdict, "a failure is not an opinion")
	assert.Contains(t, row.Error, "unexpected end of JSON input")
	assert.Equal(t, "length", row.FinishReason)

	// A failure is not remembered: the next question is asked afresh.
	res, err := svc.Evaluate(context.Background(), req, raw, "2")
	require.NoError(t, err)
	assert.Equal(t, VerdictApprove, res.Verdict)
	assert.False(t, res.Cached)
}

func TestAWordThatIsNotAVerdictIsNotAVerdict(t *testing.T) {
	for _, word := range []string{"MAYBE", "approve", "Approve", "YES", "", "APPROVE VETO"} {
		t.Run(word, func(t *testing.T) {
			model := &fakeLLM{replies: []reply{
				{content: `{"verdict": "` + word + `", "reason": "x"}`, finish: openai.FinishReasonStop},
				{content: `{"verdict": "` + word + `", "reason": "x"}`, finish: openai.FinishReasonStop},
			}}
			svc, store, _ := newTestService(model)
			req, raw := mustDecode(t)

			_, err := svc.Evaluate(context.Background(), req, raw, "r")

			require.ErrorIs(t, err, ErrUnusableReply)
			assert.Equal(t, StatusFailed, store.snapshot()[0].Status)
		})
	}
}

func TestAReplyWithNoChoicesAndANonJSONReplyAreUnusable(t *testing.T) {
	model := &fakeLLM{replies: []reply{
		{content: "Sure! I would approve this.", finish: openai.FinishReasonStop},
	}} // the second call gets no choices at all
	svc, _, _ := newTestService(model)
	req, raw := mustDecode(t)

	_, err := svc.Evaluate(context.Background(), req, raw, "r")

	require.ErrorIs(t, err, ErrUnusableReply)
	assert.Equal(t, 2, model.callCount())
}

func TestAnUnreachableModelIsAFailureAndIsNotRetried(t *testing.T) {
	model := &fakeLLM{replies: []reply{{err: errors.New("connection refused")}, good(VerdictApprove, "x")}}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	_, err := svc.Evaluate(context.Background(), req, raw, "1")

	require.ErrorIs(t, err, ErrModelUnavailable)
	assert.Equal(t, 1, model.callCount(), "the client already retries a failing call; this layer does not")
	row := store.snapshot()[0]
	assert.Equal(t, StatusFailed, row.Status)
	assert.Contains(t, row.Error, "connection refused")
}

func TestTheReasonIsCleanedAndCut(t *testing.T) {
	long := strings.Repeat("дума ", 100)
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "Първи ред\n\nВторият ред https://evil.example "+long)}}
	svc, _, _ := newTestService(model)
	req, raw := mustDecode(t)

	res, err := svc.Evaluate(context.Background(), req, raw, "r")

	require.NoError(t, err)
	assert.NotContains(t, res.Reason, "\n")
	assert.NotContains(t, res.Reason, "evil.example")
	assert.LessOrEqual(t, utf8.RuneCountInString(res.Reason), maxReasonRunes)
	assert.True(t, utf8.ValidString(res.Reason))
}

func TestALostJournalRowDoesNotLoseTheAnswer(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "Добре.")}}
	svc, store, _ := newTestService(model)
	store.failSave = true
	req, raw := mustDecode(t)

	res, err := svc.Evaluate(context.Background(), req, raw, "r")

	require.NoError(t, err)
	assert.Equal(t, VerdictApprove, res.Verdict)
	assert.Empty(t, store.snapshot())
}

func TestAMissingPromptIsAFaultOnThisSideNotAModelFailure(t *testing.T) {
	model := &fakeLLM{replies: []reply{good(VerdictApprove, "x")}}
	svc, _, _ := newTestService(model)
	svc.prompt = func() (string, error) { return "", errors.New("no such template") }
	req, raw := mustDecode(t)

	_, err := svc.Evaluate(context.Background(), req, raw, "r")

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrModelUnavailable))
	assert.False(t, errors.Is(err, ErrUnusableReply))
	assert.Equal(t, 0, model.callCount())
}

// The bot gave up on the first call (its timeout is shorter than a cold model load)
// and asks again. The second question joins the first instead of starting a second
// call to the model.
func TestTwoQuestionsAboutOneSignalShareOneCall(t *testing.T) {
	model := &fakeLLM{
		replies: []reply{good(VerdictApprove, "Да.")},
		gate:    make(chan struct{}),
		started: make(chan struct{}, 1),
	}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	results := make(chan Result, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	ask := func(id string) {
		defer wg.Done()
		res, err := svc.Evaluate(context.Background(), req, raw, id)
		results <- res
		errs <- err
	}
	wg.Add(1)
	go ask("first")
	<-model.started // the first call is at the model
	wg.Add(1)
	go ask("second")
	time.Sleep(50 * time.Millisecond) // the second one has joined (or will find the cache)
	close(model.gate)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	var cached, fresh int
	for res := range results {
		assert.Equal(t, VerdictApprove, res.Verdict)
		if res.Cached {
			cached++
		} else {
			fresh++
		}
	}
	assert.Equal(t, 1, model.callCount(), "one call to the model for two questions")
	assert.Equal(t, 1, fresh)
	assert.Equal(t, 1, cached)
	assert.Len(t, store.snapshot(), 1)
}

// The caller stops waiting (the bot's timeout). The model's answer is still
// journaled and cached, so the bot's next question gets it at once.
func TestTheAnswerIsKeptWhenTheCallerGivesUp(t *testing.T) {
	model := &fakeLLM{
		replies: []reply{good(VerdictVeto, "Не.")},
		gate:    make(chan struct{}),
		started: make(chan struct{}, 1),
	}
	svc, store, _ := newTestService(model)
	req, raw := mustDecode(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := svc.Evaluate(ctx, req, raw, "first")
		done <- err
	}()
	<-model.started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	close(model.gate) // the model answers after the caller has gone
	require.Eventually(t, func() bool { return len(store.snapshot()) == 1 }, 3*time.Second, 10*time.Millisecond)

	res, err := svc.Evaluate(context.Background(), req, raw, "second")
	require.NoError(t, err)
	assert.True(t, res.Cached)
	assert.Equal(t, VerdictVeto, res.Verdict)
	assert.Equal(t, 1, model.callCount())
}

func TestTheCacheStaysBounded(t *testing.T) {
	c := newVerdictCache()
	now := time.Date(2026, 11, 5, 15, 0, 0, 0, time.UTC)
	for i := 0; i < cacheMaxEntries+500; i++ {
		c.put(fmt.Sprintf("sig-%d", i), Result{Verdict: VerdictVeto}, now.Add(time.Duration(i)*time.Millisecond))
	}
	assert.LessOrEqual(t, len(c.items), cacheMaxEntries)
}
