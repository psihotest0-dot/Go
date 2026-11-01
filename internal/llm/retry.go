package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const (
	// maxAttempts is one initial try plus three retries.
	maxAttempts = 4
	// defaultRetryBaseDelay doubles after every failed attempt: 100ms, 200ms, 400ms.
	defaultRetryBaseDelay = 100 * time.Millisecond
	// maxErrorBodyBytes caps how much of an error response ends up in an error
	// message (and from there in the logs).
	maxErrorBodyBytes = 512
)

// StatusError is a non-2xx answer from the LLM server on the raw HTTP path
// (ChatWithJSONSchema). The go-openai calls report the same thing as
// *openai.APIError or *openai.RequestError.
type StatusError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("LLM server answered %s: %s", e.Status, e.Body)
}

// httpStatus returns the HTTP status carried by err, or 0 when the failure did
// not come from an HTTP answer (network error, bad JSON, ...).
func httpStatus(err error) int {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return reqErr.HTTPStatusCode
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode
	}
	return 0
}

// retryable reports whether repeating the request could help. A 4xx answer is
// the server saying the request itself is wrong (for example "context length
// exceeded"): sending it again only costs another slot and the same refusal.
// Timeouts (408) and rate limits (429) are the exceptions. A finished context
// is never worth another attempt.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	status := httpStatus(err)
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return false
	}
	return true
}

// retry runs attempt up to maxAttempts times with exponential backoff, stopping
// at the first success, the first non-retryable error or when ctx ends. The
// returned error wraps the last failure so callers can still inspect it with
// errors.Is / errors.As.
func retry(ctx context.Context, op string, baseDelay time.Duration, attempt func() error) error {
	var err error
	for i := 0; i < maxAttempts; i++ {
		if err = attempt(); err == nil {
			return nil
		}
		if !retryable(ctx, err) {
			return fmt.Errorf("llm %s: %w", op, err)
		}
		if i == maxAttempts-1 {
			break
		}

		select {
		case <-time.After(baseDelay * (1 << i)):
		case <-ctx.Done():
			return fmt.Errorf("llm %s: %w", op, ctx.Err())
		}
	}
	return fmt.Errorf("llm %s failed after %d attempts: %w", op, maxAttempts, err)
}
