package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Shutdown waits for the rolling summaries that chat turns started in the background, so
// that the database is not closed under them.
func TestWaitForSummariesWaitsForARunningPass(t *testing.T) {
	o := &Orchestrator{}
	o.summaryWG.Add(1)
	go func() {
		time.Sleep(60 * time.Millisecond)
		o.summaryWG.Done()
	}()

	start := time.Now()
	ok := o.WaitForSummaries(2 * time.Second)

	assert.True(t, ok)
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "it really waited for the pass")
}

func TestWaitForSummariesGivesUpAfterTheTimeout(t *testing.T) {
	o := &Orchestrator{}
	o.summaryWG.Add(1) // a pass that never ends
	t.Cleanup(o.summaryWG.Done)

	start := time.Now()
	ok := o.WaitForSummaries(40 * time.Millisecond)

	assert.False(t, ok)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestWaitForSummariesReturnsAtOnceWhenNothingRuns(t *testing.T) {
	assert.True(t, (&Orchestrator{}).WaitForSummaries(time.Second))
}
