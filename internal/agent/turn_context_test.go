package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/prompts"
)

const (
	turnContextMessage = "какво ядох вчера за вечеря"
	turnContextMemory  = "Ивайло яде пица в петък вечер."
)

// turnContextEnv is an orchestrator whose search finds turnContextMemory for turnContextMessage, and
// a full profile of other memories, so that the found one is not one of the profile.
func turnContextEnv(t *testing.T) (*attachmentEnv, *Orchestrator) {
	t.Helper()
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	create := func(content string, vector []float32, importance int32) {
		_, err := e.db.Queries.CreateMemory(context.Background(), db.CreateMemoryParams{
			UserID: e.userID, Content: content, Status: "active", Embedding: pgvector.NewVector(vector),
			EmbeddingModel: config.DefaultEmbedModel, Importance: importance, ContentHash: uuid.NewString(),
		})
		require.NoError(t, err)
	}
	for i := range topMemoryLimit {
		text := fmt.Sprintf("Профилен факт номер %d.", i)
		create(text, mockVector(t, text), 9)
	}
	create(turnContextMemory, mockVector(t, turnContextMessage), 1)
	storeReplies(t, e, "tc", 1, turnContextMessage)
	return e, o
}

func TestTheMemoriesOfATurnStayInTheSystemPromptByDefault(t *testing.T) {
	e, o := turnContextEnv(t)

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "tc", Content: turnContextMessage}, false)
	require.NoError(t, err)

	assert.Contains(t, prep.messages[0].Content, turnContextMemory)
	for _, m := range prep.messages[1:] {
		assert.NotContains(t, m.Content, turnContextMemory)
	}
}

// With CONTEXT_AFTER_HISTORY the memory goes into a system message of its own right before the
// person's message; the system prompt at the start no longer has it, nor its heading.
func TestTheMemoriesOfATurnGoAfterTheHistory(t *testing.T) {
	e, o := turnContextEnv(t)
	o.WithContextAfterHistory()

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "tc", Content: turnContextMessage}, false)
	require.NoError(t, err)

	n := len(prep.messages)
	require.GreaterOrEqual(t, n, 4, "system, the stored pair, the context, the message")
	assert.NotContains(t, prep.messages[0].Content, turnContextMemory)
	assert.NotContains(t, prep.messages[0].Content, "### Лична памет")
	turnContext := prep.messages[n-2]
	assert.Equal(t, openai.ChatMessageRoleSystem, turnContext.Role)
	assert.Contains(t, turnContext.Content, "The user did not write it.")
	assert.Contains(t, turnContext.Content, "### Лична памет за потребителя (Memories):\n"+turnContextMemory)
	last := prep.messages[n-1]
	assert.Equal(t, openai.ChatMessageRoleUser, last.Role)
	assert.Contains(t, last.Content, turnContextMessage)
	assert.NotContains(t, last.Content, turnContextMemory, "the person's message keeps its own words")
	assert.Equal(t, openai.ChatMessageRoleAssistant, prep.messages[n-3].Role, "the history comes before the context")
}

func TestATurnWithNothingFoundHasNoContextMessage(t *testing.T) {
	c := chatContext{}
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "здравей"}}
	msg, err := takeTurnContext(&c, history)
	require.NoError(t, err)
	assert.Nil(t, msg)
	assert.Equal(t, history, withTurnContext(history, nil))
}

// The context is moved only before the person's message: a history that ends with an answer
// keeps the memories and the summaries in the system prompt.
func TestTheContextStaysWhenTheLastMessageIsNotThePersons(t *testing.T) {
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })
	c := chatContext{Memories: []string{"спомен"}, Chunks: []string{"резюме"}}
	msg, err := takeTurnContext(&c, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleAssistant, Content: "ето"}})
	require.NoError(t, err)
	assert.Nil(t, msg)
	assert.Equal(t, []string{"спомен"}, c.Memories)
	assert.Equal(t, []string{"резюме"}, c.Chunks)

	moved, err := takeTurnContext(&c, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "а?"}})
	require.NoError(t, err)
	require.NotNil(t, moved)
	assert.Contains(t, moved.Content, "### Идеи и прозрения от книгите (Internalized Concepts):\nрезюме")
	assert.Nil(t, c.Memories)
	assert.Nil(t, c.Chunks)
}
