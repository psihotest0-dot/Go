package jobs

import (
	"context"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/prompts"
)

// A fact told just before the chat is deleted still becomes a memory: DeleteSession keeps
// the unread user message under a retired id, the memory job reads it as usual, and the
// periodic job then removes it.
func TestMemoryOfADeletedChatIsStillMadeAndThenTheMessageIsPurged(t *testing.T) {
	dbURL := dbtest.ConnString(t)
	t.Chdir("../..")
	prompts.Init("prompts")

	ctx := context.Background()
	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	_, err = database.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID: user.ID, SessionID: "chat-a", Role: "user", Content: "Живея във Варна",
	})
	require.NoError(t, err)

	deleted, err := database.DeleteSession(ctx, user.ID, "chat-a")
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted.Retired, "the unread message is kept for the memory job")

	llmMock := &mockLLMClient{responses: []string{
		`[{"fact_key": "location", "value": "Varna", "statement": "Живея във Варна", "is_explicit_change": false}]`,
	}}
	worker := NewMemoryConsolidationWorker(database, llmMock, &mockEmbedder{})
	require.NoError(t, worker.Work(ctx, &river.Job[MemoryConsolidationArgs]{Args: MemoryConsolidationArgs{UserID: user.ID}}))

	mems, err := database.Queries.GetMemories(ctx, db.GetMemoriesParams{UserID: user.ID})
	require.NoError(t, err)
	require.Len(t, mems, 1, "the fact from the deleted chat became a memory")
	assert.Equal(t, "Живея във Варна", mems[0].Content)

	// The periodic job removes what has been read.
	NewMemoryConsolidationPeriodicWorker(database).purgeRetired(ctx)

	var left int
	require.NoError(t, database.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE user_id=$1", user.ID).Scan(&left))
	assert.Zero(t, left, "nothing of the deleted chat remains")
}
