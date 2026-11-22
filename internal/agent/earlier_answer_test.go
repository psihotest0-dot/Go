package agent

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
)

func answerWith(content string, ids ...uuid.UUID) db.Message {
	var stored []api.DBChunkInfo
	for _, id := range ids {
		stored = append(stored, api.DBChunkInfo{ChunkID: id})
	}
	raw, _ := json.Marshal(stored)
	return db.Message{Role: "assistant", Content: content, SourceChunkIds: pqtype.NullRawMessage{RawMessage: raw, Valid: true}}
}

func TestTheEarlierAnswerIsTheLastOneWithSources(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	user := func(s string) db.Message { return db.Message{Role: "user", Content: s} }

	got := earlierAnswerOf([]db.Message{
		user("А какво още има в нея?"),
		answerWith("Марк Аврелий пише…", a, b, a),
		user("Какво казва Марк Аврелий за смъртта?"),
	})
	assert.Equal(t, earlierAnswer{question: "Какво казва Марк Аврелий за смъртта?", chunkIDs: []uuid.UUID{a, b}}, got, "each source once, in order")

	got = earlierAnswerOf([]db.Message{
		user("а още?"),
		answerWith("Добре съм."),
		user("как си"),
		answerWith("Рецептата е…", c),
		user("Как се приготвя баница?"),
	})
	assert.Equal(t, []uuid.UUID{c}, got.chunkIDs, "the answer before a small talk still counts")
	assert.Equal(t, "Как се приготвя баница?", got.question)

	assert.Empty(t, earlierAnswerOf([]db.Message{
		user("а още?"), answerWith("три"), user("x"), answerWith("две"), user("y"), answerWith("едно", c), user("z"),
	}).chunkIDs, "only the last two answers are looked at")
	assert.Empty(t, earlierAnswerOf([]db.Message{user("здравей")}).chunkIDs)
	assert.Empty(t, earlierAnswerOf([]db.Message{user("а?"), {Role: "assistant", Content: "стар запис"}}).chunkIDs, "an answer stored without sources")
}

func TestAMessageThatAsksForMore(t *testing.T) {
	for _, m := range []string{"А какво още има в нея?", "разкажи ми още", "кажи още", "продължи", "Има ли още нещо там?", "още за смъртта", "какво друго пише", "What else is in it?", "tell me more"} {
		assert.True(t, asksForMore.MatchString(m), m)
	}
	for _, m := range []string{"Какво казва Марк Аврелий за смъртта?", "още ли спиш", "благодаря", "Как се приготвя баница?", "продуктът е добър"} {
		assert.False(t, asksForMore.MatchString(m), m)
	}
}

func TestThePassagesOfTheEarlierAnswerThatAreNotFoundAgain(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	passages := []earlierPassage{{ID: a}, {ID: b}}
	assert.Equal(t, []earlierPassage{{ID: b}}, without(passages, []retrieval.SearchResult{{ID: a}}))
	assert.Equal(t, passages, without(passages, nil))
}
