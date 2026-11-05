package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"golang.org/x/sync/singleflight"

	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/metrics"
)

const (
	// modelTimeout bounds one question with its repeat. The bot waits much less (about
	// ten seconds); the work goes on after it has given up, so that its next ask
	// finds the answer in the cache. A cold model load is the case this is for.
	modelTimeout = 40 * time.Second

	// maxAttempts is the number of times the model is asked while its reply cannot be
	// used. Sampling is random: on 2026-11-05 one distillation reply looped until the
	// token limit cut it off, and the same input sent again came back normal every
	// time. The answer here is short, so a repeat is cheap.
	maxAttempts = 2

	// maxAnswerTokens is room for the verdict and a sentence of Bulgarian (about 3
	// characters a token) with the JSON around it; a loop is cut off early.
	maxAnswerTokens = 200
	maxReasonRunes  = 200

	// Low, so that the same facts get the same verdict; a loop is what the repeat is for.
	temperature = 0.2

	// A HOLD means "not now" and is meant to be asked again; a few minutes of the same
	// answer spare the model the questions of the ticks in between. A final verdict is
	// kept longer than a signal lives.
	holdCacheTTL    = 3 * time.Minute
	finalCacheTTL   = 30 * time.Minute
	cacheMaxEntries = 2048

	journalTimeout = 5 * time.Second
)

var (
	// ErrModelUnavailable: the model server did not answer (down, loading, timed out).
	ErrModelUnavailable = errors.New("the model did not answer")
	// ErrUnusableReply: the model answered twice, and neither reply was a verdict.
	ErrUnusableReply = errors.New("the model's reply cannot be used")
)

// Service asks the model for a verdict and keeps the journal.
type Service struct {
	llm    llm.LLMClient
	store  Store
	prompt func() (string, error)
	now    func() time.Time
	cache  *verdictCache
	flight singleflight.Group
}

// NewService returns the Oracle that asks client and writes to store. The model is
// asked at the priority of the chat, not of the background work: a signal does not
// wait for a distillation.
func NewService(client llm.LLMClient, store Store) *Service {
	return &Service{
		llm:    client,
		store:  store,
		prompt: renderSystemPrompt,
		now:    time.Now,
		cache:  newVerdictCache(),
	}
}

func renderSystemPrompt() (string, error) {
	if prompts.GlobalManager == nil {
		return "", errors.New("the prompt templates are not initialised")
	}
	return prompts.GlobalManager.Render("oracle_system.tmpl", nil)
}

// Evaluate returns the verdict for the request. raw is the body as received, for the
// journal. A verdict given a moment ago for the same signal is repeated from memory,
// and two questions about one signal at the same time share one call to the model.
//
// The error is ErrModelUnavailable or ErrUnusableReply (both can wrap a context
// error), the context's own error if the caller gave up while waiting, or another
// error for a fault on this side.
func (s *Service) Evaluate(ctx context.Context, req *Request, raw []byte, requestID string) (Result, error) {
	key := req.Signal.SignalID
	if res, ok := s.cache.get(key, s.now()); ok {
		metrics.OracleRequests.WithLabelValues("cached").Inc()
		res.RequestID = requestID
		return res, nil
	}

	// The question is asked on a context of its own: when the bot stops waiting, the
	// answer still lands in the cache and the journal, and its next question finds it.
	//
	// Shared is true for the caller that started the call as well as for those that
	// joined it, so the one that ran the function is told apart by a flag the function
	// sets: only a caller that joined gets "cached".
	var led bool
	results := s.flight.DoChan(key, func() (any, error) {
		led = true
		return s.ask(context.WithoutCancel(ctx), req, raw, requestID)
	})
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case r := <-results:
		if r.Err != nil {
			return Result{}, r.Err
		}
		res := r.Val.(Result)
		res.RequestID = requestID
		if r.Shared && !led { // led is read after the result arrived, which is after the function returned
			res.Cached = true
			metrics.OracleRequests.WithLabelValues("cached").Inc()
		}
		return res, nil
	}
}

// ask goes to the model, writes the journal and fills the cache.
func (s *Service) ask(ctx context.Context, req *Request, raw []byte, requestID string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, modelTimeout)
	defer cancel()

	started := time.Now()
	ev := Evaluation{
		RequestID: requestID,
		SignalID:  req.Signal.SignalID,
		Symbol:    req.Signal.Symbol,
		Mode:      req.Mode,
		Request:   raw,
	}

	answer, err := s.query(ctx, req, &ev)
	elapsed := time.Since(started)
	ev.LatencyMS = int(elapsed.Milliseconds())
	metrics.OracleLatency.Observe(elapsed.Seconds())

	if err != nil {
		ev.Status = StatusFailed
		ev.Error = cleanText(err.Error(), maxFieldRunes)
		metrics.OracleRequests.WithLabelValues("failed").Inc()
		slog.ErrorContext(ctx, "oracle evaluation failed",
			slog.String("signal_id", ev.SignalID), slog.String("symbol", ev.Symbol),
			slog.Int("attempts", ev.Attempts), slog.Int64("latency_ms", elapsed.Milliseconds()),
			slog.Any("error", err))
		s.journal(ctx, ev)
		return Result{}, err
	}

	ev.Status = StatusAnswered
	ev.Verdict = answer.Verdict
	ev.Reason = answer.Reason
	metrics.OracleRequests.WithLabelValues("answered").Inc()
	metrics.OracleVerdicts.WithLabelValues(answer.Verdict).Inc()
	slog.InfoContext(ctx, "oracle evaluated",
		slog.String("signal_id", ev.SignalID), slog.String("symbol", ev.Symbol),
		slog.String("verdict", ev.Verdict), slog.String("mode", ev.Mode),
		slog.String("finish_reason", ev.FinishReason), slog.Int("attempts", ev.Attempts),
		slog.Int("prompt_tokens", ev.PromptTokens), slog.Int("completion_tokens", ev.CompletionTokens),
		slog.Int64("latency_ms", elapsed.Milliseconds()))
	s.journal(ctx, ev)

	res := Result{
		Verdict:   answer.Verdict,
		Reason:    answer.Reason,
		RequestID: requestID,
		Model:     ev.Model,
		LatencyMS: elapsed.Milliseconds(),
	}
	cached := res
	cached.Cached = true
	cached.LatencyMS = 0
	s.cache.put(ev.SignalID, cached, s.now())
	return res, nil
}

// query asks the model up to maxAttempts times until a reply is a verdict. It fills
// the model, token, attempt and finish-reason fields of ev.
func (s *Service) query(ctx context.Context, req *Request, ev *Evaluation) (Answer, error) {
	system, err := s.prompt()
	if err != nil {
		return Answer{}, fmt.Errorf("render the Oracle prompt: %w", err)
	}
	chat := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: system},
			{Role: openai.ChatMessageRoleUser, Content: req.userMessage()},
		},
		MaxTokens:   maxAnswerTokens,
		Temperature: temperature,
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ev.Attempts = attempt
		resp, err := s.llm.ChatWithJSONSchema(ctx, chat, Answer{})
		if err != nil {
			return Answer{}, fmt.Errorf("%w: %w", ErrModelUnavailable, err)
		}
		ev.Model = resp.Model
		ev.PromptTokens += resp.Usage.PromptTokens
		ev.CompletionTokens += resp.Usage.CompletionTokens
		ev.FinishReason = ""
		if len(resp.Choices) > 0 {
			ev.FinishReason = string(resp.Choices[0].FinishReason)
		}

		answer, perr := parseAnswer(resp)
		if perr == nil {
			return answer, nil
		}
		lastErr = perr
		slog.WarnContext(ctx, "oracle reply not usable",
			slog.String("signal_id", ev.SignalID), slog.Int("attempt", attempt), slog.Int("attempts", maxAttempts),
			slog.String("finish_reason", ev.FinishReason), slog.Any("error", perr),
			slog.String("reply", replyExcerpt(resp)))
	}
	return Answer{}, fmt.Errorf("%w: %w", ErrUnusableReply, lastErr)
}

// parseAnswer returns the verdict in the reply. Only the three words count; the
// reason is cleaned and cut.
func parseAnswer(resp openai.ChatCompletionResponse) (Answer, error) {
	if len(resp.Choices) == 0 {
		return Answer{}, errors.New("the reply has no choices")
	}
	var a Answer
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Choices[0].Message.Content)), &a); err != nil {
		return Answer{}, err
	}
	switch a.Verdict {
	case VerdictApprove, VerdictVeto, VerdictHold:
	default:
		return Answer{}, errors.New("the verdict is not APPROVE, VETO or HOLD")
	}
	a.Reason = cleanText(a.Reason, maxReasonRunes)
	return a, nil
}

func replyExcerpt(resp openai.ChatCompletionResponse) string {
	if len(resp.Choices) == 0 {
		return ""
	}
	return cleanText(resp.Choices[0].Message.Content, 200)
}

// journal writes a row. A lost row is logged and does not lose the answer.
func (s *Service) journal(ctx context.Context, ev Evaluation) {
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalTimeout)
	defer cancel()
	if _, err := s.store.Save(jctx, ev); err != nil {
		slog.ErrorContext(ctx, "failed to journal the oracle evaluation",
			slog.String("signal_id", ev.SignalID), slog.Any("error", err))
	}
}

// verdictCache remembers the verdict of a signal for a while.
type verdictCache struct {
	mu    sync.Mutex
	items map[string]cacheEntry
}

type cacheEntry struct {
	res     Result
	expires time.Time
}

func newVerdictCache() *verdictCache { return &verdictCache{items: map[string]cacheEntry{}} }

func (c *verdictCache) get(key string, now time.Time) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return Result{}, false
	}
	if !now.Before(e.expires) {
		delete(c.items, key)
		return Result{}, false
	}
	return e.res, true
}

func (c *verdictCache) put(key string, res Result, now time.Time) {
	ttl := finalCacheTTL
	if res.Verdict == VerdictHold {
		ttl = holdCacheTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= cacheMaxEntries {
		var oldest string
		var oldestAt time.Time
		for k, e := range c.items {
			if !now.Before(e.expires) {
				delete(c.items, k)
				continue
			}
			if oldest == "" || e.expires.Before(oldestAt) {
				oldest, oldestAt = k, e.expires
			}
		}
		if len(c.items) >= cacheMaxEntries && oldest != "" {
			delete(c.items, oldest)
		}
	}
	c.items[key] = cacheEntry{res: res, expires: now.Add(ttl)}
}
