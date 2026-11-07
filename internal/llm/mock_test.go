package llm

import (
	"context"
	"sync"
	"testing"

	"github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The orchestrator streams a chat turn while the rolling summary of the same
// session asks the same model for a summary in the background. A mock that
// records its requests without a lock turns that into a data race, and the
// race detector (CI runs it) fails whichever test happens to be streaming.
// This test is only meaningful under -race: without the lock it reports a race
// on every run.
func TestMockLLMRecordsRequestsFromSeveralGoroutines(t *testing.T) {
	m := &MockLLM{Response: openai.ChatCompletionResponse{ID: "r"}}
	const rounds = 8

	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, err := m.Chat(context.Background(), openai.ChatCompletionRequest{Model: "chat"})
			assert.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			_, err := m.ChatWithJSONSchema(context.Background(), openai.ChatCompletionRequest{Model: "json"}, map[string]any{"type": "object"})
			assert.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			stream, err := m.ChatStream(context.Background(), openai.ChatCompletionRequest{Model: "stream"})
			if assert.NoError(t, err) {
				assert.NoError(t, stream.Close())
			}
		}()
	}
	wg.Wait()

	require.Len(t, m.Requests, rounds, "every streamed request is recorded exactly once")
}
