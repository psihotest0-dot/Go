package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
)

// The earlier messages of the person for the search of memories: the newest message of the person
// is the one of this turn and is not one of them, the answers are not either (plan step 0.5).
func TestTheEarlierMessagesOfThePersonLeaveOutThisTurnAndTheAnswers(t *testing.T) {
	m := func(role, content string) db.Message { return db.Message{Role: role, Content: content} }
	newestFirst := []db.Message{
		m("user", "а как да се справя?"),
		m("assistant", "Звучи изморително."),
		m("user", "цяла седмица съм нощем"),
		m("assistant", "Пак ли?"),
		m("user", "днес пак нощна смяна"),
		m("user", "трето"),
		m("user", "четвърто"),
	}
	assert.Equal(t, []string{"цяла седмица съм нощем", "днес пак нощна смяна", "трето"}, earlierUserMessages(newestFirst))

	// Regenerate: the answer being replaced is newer than the message of the turn.
	assert.Equal(t, []string{"цяла седмица съм нощем"}, earlierUserMessages([]db.Message{
		m("assistant", "Старият отговор."), m("user", "а как да се справя?"), m("user", "цяла седмица съм нощем"),
	}))
	assert.Empty(t, earlierUserMessages([]db.Message{m("user", "здравей")}), "the first message of a conversation has nothing before it")
	assert.Empty(t, earlierUserMessages(nil))
}

// recordingEmbedder embeds every text as the first axis and keeps the texts.
type recordingEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (e *recordingEmbedder) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	e.mu.Lock()
	e.texts = append(e.texts, texts...)
	e.mu.Unlock()
	return questionAtOrigin{}.EmbedTexts(context.Background(), texts, false)
}

// The turn searches the memories of a short message with the person's earlier messages of the
// conversation, read from the database (the message of the turn is already stored).
func TestAShortMessageIsSearchedWithTheEarlierMessagesOfItsConversation(t *testing.T) {
	e := newContextEnv(t)
	embedder := &recordingEmbedder{}
	e.o.retrieval = retrieval.NewRetrievalService(e.d, embedder)
	ctx := context.Background()
	for _, m := range []struct{ role, content string }{
		{"user", "днес пак нощна смяна"}, {"assistant", "Пак ли?"}, {"user", "а как да се справя?"},
	} {
		_, err := e.d.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: e.userID, SessionID: "session", Role: m.role, Content: m.content})
		require.NoError(t, err)
	}

	_, _, err := e.o.loadContext(ctx, e.userID, ChatRequest{SessionID: "session", Content: "а как да се справя?"})
	require.NoError(t, err)

	assert.Contains(t, embedder.texts, "а как да се справя?", "the message alone is searched")
	assert.Contains(t, embedder.texts, "а как да се справя?\nднес пак нощна смяна", "and with the earlier message of the person")
}
