package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
)

// book stores a finished file with n passages and returns their ids, in order.
func (e *toolEnv) book(t *testing.T, name string, n int) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	file, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: e.userID, FileName: name, FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)
	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i := 0; i < n; i++ {
		v := make([]float32, config.VectorDimension)
		v[0], v[1+i%5] = 1, 0.5
		arg.FileIds = append(arg.FileIds, file.ID)
		arg.UserIds = append(arg.UserIds, e.userID)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, fmt.Sprintf("%s, откъс %d", name, i))
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(v))
		arg.ChunkTypes = append(arg.ChunkTypes, "raw_text")
		arg.Metadatas = append(arg.Metadatas, "{}")
	}
	require.NoError(t, e.db.Queries.CreateDocumentChunksBatch(ctx, arg))
	rows, err := e.db.SqlDB.QueryContext(ctx, `SELECT id FROM document_chunks WHERE file_id = $1 ORDER BY chunk_index`, file.ID)
	require.NoError(t, err)
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	return ids
}

type searchedPassage struct {
	id      uuid.UUID
	file    string
	earlier bool
}

// askAfter runs search_documents for message, with earlier as the answer before it.
func (e *toolEnv) askAfter(t *testing.T, message string, earlier earlierAnswer) ([]searchedPassage, *turnState, map[string]interface{}) {
	t.Helper()
	ctx, turn := e.turn("s", false)
	turn.setEarlierAnswer(earlier)
	res, err := e.reg.ExecuteTool(WithCurrentQuery(ctx, message), searchDocumentsTool, map[string]interface{}{"query": message})
	require.NoError(t, err)
	out := res.(map[string]interface{})
	var got []searchedPassage
	for _, r := range out["results"].([]map[string]interface{}) {
		got = append(got, searchedPassage{id: r["id"].(uuid.UUID), file: r["file_name"].(string), earlier: r["from_earlier_answer"] == true})
	}
	return got, turn, out
}

// "А какво още има в нея?" after an answer from a book: the new passages of that book, none of
// the ones the answer used; the sources panel lists the new ones only (plan step 1.1).
func TestAskingForMoreSearchesTheBookOfTheEarlierAnswer(t *testing.T) {
	e := newToolEnv(t)
	aurelius := e.book(t, "Марк Аврелий - Към себе си.pdf", 6)
	seneca := e.book(t, "Сенека - Писма.pdf", 4)
	e.book(t, "Готварска книга.pdf", 6)

	got, turn, out := e.askAfter(t, "А какво още има в нея?", earlierAnswer{
		question: "Какво казва Марк Аврелий за смъртта?",
		chunkIDs: []uuid.UUID{aurelius[0], aurelius[1], seneca[0]},
	})
	require.Len(t, got, 4, "the four passages of the book the answer did not use")
	for _, p := range got {
		assert.Equal(t, "Марк Аврелий - Към себе си.pdf", p.file)
		assert.False(t, p.earlier)
		assert.NotContains(t, []uuid.UUID{aurelius[0], aurelius[1]}, p.id, "a passage of the earlier answer is not new")
	}
	assert.NotContains(t, out, "note")
	assert.Len(t, turn.foundSources(), 4)
}

// When the book has few new passages, the passages of the earlier answer come too, marked, and
// not as sources; of two books with as many passages the one ranked first is the book.
func TestTheEarlierPassagesComeMarkedWhenTheBookHasLittleLeft(t *testing.T) {
	e := newToolEnv(t)
	seneca := e.book(t, "Сенека - Писма.pdf", 4)
	aurelius := e.book(t, "Марк Аврелий - Към себе си.pdf", 6)

	got, turn, out := e.askAfter(t, "разкажи ми още", earlierAnswer{
		question: "Какво казва Сенека за гнева?",
		chunkIDs: []uuid.UUID{seneca[0], aurelius[0], seneca[1], aurelius[1]},
	})
	var fresh, marked []searchedPassage
	for _, p := range got {
		if p.earlier {
			marked = append(marked, p)
		} else {
			fresh = append(fresh, p)
		}
	}
	require.Len(t, fresh, 2, "two of each book: the one ranked first is the book")
	for _, p := range fresh {
		assert.Equal(t, "Сенека - Писма.pdf", p.file)
	}
	assert.ElementsMatch(t, []uuid.UUID{seneca[0], aurelius[0], seneca[1], aurelius[1]},
		[]uuid.UUID{marked[0].id, marked[1].id, marked[2].id, marked[3].id})
	assert.Equal(t, earlierPassagesNote, out["note"])
	assert.Len(t, turn.foundSources(), 2, "the passages of the earlier answer are not sources of this one")
}

// A message that does not ask for more is searched as before, the earlier answer or not.
func TestANewQuestionIsNotTiedToTheEarlierBook(t *testing.T) {
	e := newToolEnv(t)
	aurelius := e.book(t, "Марк Аврелий - Към себе си.pdf", 6)
	e.book(t, "Готварска книга.pdf", 6)

	got, _, out := e.askAfter(t, "Как се приготвя баница?", earlierAnswer{question: "Какво казва Марк Аврелий за смъртта?", chunkIDs: aurelius[:2]})
	files := map[string]bool{}
	for _, p := range got {
		files[p.file] = true
		assert.False(t, p.earlier)
	}
	assert.NotEqual(t, map[string]bool{"Марк Аврелий - Към себе си.pdf": true}, files, "the search is not kept inside the earlier book")
	assert.NotContains(t, out, "note")
}

// Through a whole turn: the earlier answer comes from the stored conversation, and the answer to
// "А какво още има в нея?" is stored with the new passages of that book as its sources.
func TestATurnThatAsksForMoreGetsTheNewPassagesOfTheBook(t *testing.T) {
	e := newToolEnv(t)
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })
	aurelius := e.book(t, "Марк Аврелий - Към себе си.pdf", 6)
	e.book(t, "Готварска книга.pdf", 6)
	ctx := context.Background()
	const session = "more"
	_, err := e.db.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: e.userID, SessionID: session, Role: "user", Content: "Какво казва Марк Аврелий за смъртта?",
		SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true}})
	require.NoError(t, err)
	_, err = e.db.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: e.userID, SessionID: session, Role: "assistant", Content: "Той пише, че смъртта е природа.",
		SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte(fmt.Sprintf(`[{"chunk_id":%q},{"chunk_id":%q}]`, aurelius[0], aurelius[1])), Valid: true}})
	require.NoError(t, err)

	model := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", searchDocumentsTool, `{"query":"какво още има в нея"}`)},
		{textChunk("Пише още за времето.")},
	}}
	o := NewOrchestrator(e.db, retrieval.NewRetrievalService(e.db, &llm.MockEmbedder{}), model, e.reg)
	_, err = o.ProcessChatTurn(ctx, e.userID, ChatRequest{SessionID: session, Content: "А какво още има в нея?"})
	require.NoError(t, err)

	stored := assistantMessages(t, e, session)
	require.Len(t, stored, 2)
	ids := chunkIDsOf(stored[len(stored)-1])
	require.Len(t, ids, 4, "the four passages of the book the earlier answer did not use")
	assert.ElementsMatch(t, aurelius[2:], ids)
}
