package jobs

import (
	"context"
	"testing"

	"go-monolith/internal/db"
	"github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
    "github.com/riverqueue/river"
	"os"
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

func (m *mockEmbedder) EmbedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	return [][]float32{make([]float32, 384)}, nil
}

func TestMemoryConsolidationWorker(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)

	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)

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

	mems1, err := database.Queries.GetMemories(ctx, user.ID)
	require.NoError(t, err)
	assert.Len(t, mems1, 1)
	assert.Equal(t, "candidate", mems1[0].Status) // First fact is candidate

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

	mems2, err := database.Queries.GetMemories(ctx, user.ID)
	require.NoError(t, err)
	assert.Len(t, mems2, 2)

	var superseded, active bool
	for _, m := range mems2 {
		if m.Status == "superseded" {
			superseded = true
			assert.Equal(t, "Живея в София", m.Content)
		} else if m.Status == "active" {
			active = true
			assert.Equal(t, "Преместих се във Варна", m.Content)
		}
	}
	assert.True(t, superseded, "Old memory should be superseded")
	assert.True(t, active, "New memory should be active")
}
