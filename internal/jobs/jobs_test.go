package jobs

import (
	"context"
	"go-monolith/internal/config"
	"testing"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/prompts"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockLLMClient struct {
	responses []string
	callCount int
}

func (m *mockLLMClient) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return openai.ChatCompletionResponse{}, nil
}

func (m *mockLLMClient) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error) {
	return nil, nil
}

func (m *mockLLMClient) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error) {
	if m.callCount >= len(m.responses) {
		return openai.ChatCompletionResponse{}, nil
	}
	resp := m.responses[m.callCount]
	m.callCount++
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Content: resp}},
		},
	}, nil
}

type mockEmbedder struct{}

func (m *mockEmbedder) EmbedTexts(ctx context.Context, texts []string, isQuery bool) ([][]float32, error) {
	return [][]float32{make([]float32, config.VectorDimension)}, nil
}

func TestMemoryConsolidationInsertOptsAllowsReenqueueAfterCompletion(t *testing.T) {
	opts := memoryConsolidationInsertOpts()

	require.NotNil(t, opts)
	assert.Equal(t, "memory", opts.Queue)
	assert.True(t, opts.UniqueOpts.ByArgs, "jobs for the same user must still be deduplicated by args")

	// The regression: river's default state set includes JobStateCompleted, so a
	// finished job keeps its unique key and every later insert for the same user
	// is silently dropped. Consolidation then runs once per user, ever.
	assert.NotContains(t, opts.UniqueOpts.ByState, rivertype.JobStateCompleted,
		"a completed job must stop blocking new consolidation jobs")
	assert.NotContains(t, opts.UniqueOpts.ByState, rivertype.JobStateCancelled)
	assert.NotContains(t, opts.UniqueOpts.ByState, rivertype.JobStateDiscarded)

	// Duplicates must still be suppressed while a previous run is in flight.
	assert.Contains(t, opts.UniqueOpts.ByState, rivertype.JobStateAvailable)
	assert.Contains(t, opts.UniqueOpts.ByState, rivertype.JobStateRetryable)

	// River rejects a custom state set that omits a state every job must pass
	// through, so a missing one would break enqueueing at runtime.
	for _, state := range []rivertype.JobState{
		rivertype.JobStatePending,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	} {
		assert.Contains(t, opts.UniqueOpts.ByState, state)
	}
}

func TestMemoryConsolidationWorker(t *testing.T) {
	dbURL := dbtest.ConnString(t)

	// prompts.GlobalManager is initialised by the application bootstrap, and
	// the worker renders its instructions through it. The template directory is
	// resolved relative to the working directory, which is the package dir when
	// running `go test`, so move to the module root first.
	t.Chdir("../..")
	prompts.Init("prompts")

	ctx := context.Background()
	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)

	// Delete only the user this test created, so the shared test database
	// keeps whatever the other tests are using.
	defer func() {
		dbtest.DeleteUser(t, database.Pool, user.ID)
	}()

	llmMock := &mockLLMClient{
		responses: []string{
			`[{"fact_key": "location", "value": "Sofia", "statement": "Живея в София", "is_explicit_change": false}]`,
			`[{"fact_key": "location", "value": "Varna", "statement": "Преместих се във Варна", "is_explicit_change": true}]`,
		},
	}
	embedderMock := &mockEmbedder{}

	worker := NewMemoryConsolidationWorker(database, llmMock, embedderMock)

	// Message 1
	sessionID := "sess-1"
	_, err = database.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID:    user.ID,
		SessionID: sessionID,
		Role:      "user",
		Content:   "Живея в София",
	})
	require.NoError(t, err)

	err = worker.Work(ctx, &river.Job[MemoryConsolidationArgs]{
		Args: MemoryConsolidationArgs{UserID: user.ID},
	})
	require.NoError(t, err)

	mems1, err := database.Queries.GetMemories(ctx, db.GetMemoriesParams{UserID: user.ID})
	require.NoError(t, err)
	assert.Len(t, mems1, 1)
	// A fact with no conflicting memory is stored as "active"; "candidate" is
	// reserved for a silent change against an existing fact (processFact).
	assert.Equal(t, "active", mems1[0].Status)

	// Message 2
	_, err = database.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID:    user.ID,
		SessionID: sessionID,
		Role:      "user",
		Content:   "Преместих се във Варна",
	})
	require.NoError(t, err)

	err = worker.Work(ctx, &river.Job[MemoryConsolidationArgs]{
		Args: MemoryConsolidationArgs{UserID: user.ID},
	})
	require.NoError(t, err)

	mems2, err := database.Queries.GetMemories(ctx, db.GetMemoriesParams{UserID: user.ID})
	require.NoError(t, err)
	assert.Len(t, mems2, 2)

	var superseded, active bool
	for _, m := range mems2 {
		switch m.Status {
		case "superseded":
			superseded = true
			assert.Equal(t, "Живея в София", m.Content)
		case "active":
			active = true
			assert.Equal(t, "Преместих се във Варна", m.Content)
		}
	}
	assert.True(t, superseded, "Old memory should be superseded")
	assert.True(t, active, "New memory should be active")
}
