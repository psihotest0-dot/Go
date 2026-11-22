package retrieval_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/retrieval"
)

// topicWord is a word of a query and the axis it puts the query on.
type topicWord struct {
	word string
	axis int
}

// topicEmbedder puts a query on the axis of the first word of its list that the query holds, and
// counts the queries it embedded.
type topicEmbedder struct {
	words []topicWord
	mu    sync.Mutex
	seen  []string
}

func (e *topicEmbedder) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		e.mu.Lock()
		e.seen = append(e.seen, text)
		e.mu.Unlock()
		v := make([]float32, config.VectorDimension)
		v[config.VectorDimension-1] = 1
		for _, w := range e.words {
			if strings.Contains(text, w.word) {
				v = make([]float32, config.VectorDimension)
				v[w.axis] = 1
				break
			}
		}
		out[i] = v
	}
	return out, nil
}

// A short message finds the memory of its conversation through the earlier messages, and keeps
// the one it finds alone (plan step 0.5).
func TestAShortMessageFindsTheMemoryOfItsConversation(t *testing.T) {
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, db.RunMigrations(connString))
	ctx := context.Background()
	d, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(d.Close)
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, d.Pool, user.ID) })

	memory := func(content string, axis int) uuid.UUID {
		v := make([]float32, config.VectorDimension)
		v[axis] = 1
		m, err := d.CreateMemoryWithCheck(ctx, db.CreateMemoryParams{
			UserID: user.ID, Content: content, Status: "active", Importance: 5,
			Embedding: pgvector.NewVector(v), EmbeddingModel: config.DefaultEmbedModel,
		})
		require.NoError(t, err)
		return m.ID
	}
	shifts := memory("Ивайло работи на смени.", 1)
	cat := memory("Ивайло има котка Мърка.", 2)

	// "справя" alone is about the cat (axis 2); with "смяна" from earlier it is about the shifts.
	embedder := &topicEmbedder{words: []topicWord{{"смяна", 1}, {"справя", 2}}}
	svc := retrieval.NewRetrievalService(d, embedder)
	earlier := []string{"цяла седмица съм нощем", "днес пак нощна смяна"}

	got, _, err := svc.SearchForContext(ctx, user.ID, "а как да се справя?", earlier, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []uuid.UUID{cat, shifts}, []uuid.UUID{got[0].ID, got[1].ID},
		"the memory of the message alone first, then the one of the conversation")

	embedder.seen = nil
	long := "кажи ми нещо повече за котката ми, моля те, интересно ми е"
	_, _, err = svc.SearchForContext(ctx, user.ID, long, earlier, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{long}, embedder.seen, "a long message is searched alone")
}
