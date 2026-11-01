package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The context can end while a failed answer is on its way back: the attempt
// reports a 500, and by the time retry looks at it the context is done. The
// caller asked for the call to stop, so the error has to say so
// (errors.Is(err, context.Canceled)), and the failure that was seen last must
// stay inspectable. Reading the error from a real server made this depend on
// timing, which is how it showed up as a test that failed now and then on a slow
// CI machine.
func TestRetryReportsTheEndedContextWhenAnAttemptFailedAtTheSameTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failure := &StatusError{StatusCode: 500, Status: "500 Internal Server Error", Body: "boom"}

	attempts := 0
	err := retry(ctx, "chat", time.Minute, func() error {
		attempts++
		cancel() // the caller gives up while this attempt is failing
		return failure
	})

	require.Error(t, err)
	assert.Equal(t, 1, attempts, "a finished context is never worth another attempt")
	assert.ErrorIs(t, err, context.Canceled)
	var status *StatusError
	require.True(t, errors.As(err, &status), "the failure itself is still there to inspect")
	assert.Equal(t, 500, status.StatusCode)
}

func TestRetryReportsADeadlineThatPassedDuringAFailedAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	err := retry(ctx, "chat", time.Minute, func() error {
		<-ctx.Done()
		return &StatusError{StatusCode: 503, Status: "503 Service Unavailable"}
	})

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// An error that already is the context error is not wrapped twice.
func TestRetryKeepsAContextErrorFromTheAttemptAsItIs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := retry(ctx, "chat", time.Minute, func() error { return context.Canceled })

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "llm chat: context canceled", err.Error())
}

// With a live context nothing changes: the last failure is reported as it was.
func TestRetryWithALiveContextStillReportsTheLastFailure(t *testing.T) {
	failure := &StatusError{StatusCode: 400, Status: "400 Bad Request"}

	err := retry(context.Background(), "chat", time.Millisecond, func() error { return failure })

	assert.ErrorIs(t, err, failure)
	assert.NotErrorIs(t, err, context.Canceled)
}
