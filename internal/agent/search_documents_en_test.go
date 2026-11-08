package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func scoredPassage(id uuid.UUID, score float64) retrieval.SearchResult {
	return retrieval.SearchResult{ID: id, Score: score, SourceType: "chunk"}
}

func TestMergePassagesOrdersByScoreListsAPassageOnceAndStopsAtTheLimit(t *testing.T) {
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	first := []retrieval.SearchResult{scoredPassage(a, 0.90), scoredPassage(b, 0.80)}
	second := []retrieval.SearchResult{scoredPassage(b, 0.95), scoredPassage(c, 0.85)}

	ids := func(rs []retrieval.SearchResult) []uuid.UUID {
		var out []uuid.UUID
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}

	merged := mergePassages(8, first, second)
	assert.Equal(t, []uuid.UUID{b, a, c}, ids(merged), "best score first; b is listed once")
	assert.InDelta(t, 0.95, merged[0].Score, 1e-9, "a passage found by both keeps the higher score")

	assert.Equal(t, []uuid.UUID{b, a}, ids(mergePassages(2, first, second)), "at most the limit")
	assert.Equal(t, []uuid.UUID{b, a, c}, ids(mergePassages(0, first, second)), "no limit given: nothing is cut")

	tie := mergePassages(8, []retrieval.SearchResult{scoredPassage(d, 0.9)}, []retrieval.SearchResult{scoredPassage(a, 0.9)})
	assert.Equal(t, []uuid.UUID{d, a}, ids(tie), "equal scores keep the order of the searches")

	assert.Empty(t, mergePassages(8, nil, nil))
	assert.Equal(t, ids(first), ids(mergePassages(8, first, nil)), "an empty second search changes nothing")
}

func TestSearchDocumentsSchemaOffersTheEnglishQueryAsAnOption(t *testing.T) {
	for _, tool := range toolDefinitions() {
		if tool.Function.Name != "search_documents" {
			continue
		}
		parameters, _ := tool.Function.Parameters.(map[string]interface{})
		properties, _ := parameters["properties"].(map[string]interface{})
		require.Contains(t, properties, queryENArg)
		// "Always give it" is what made the model fill the argument in 34 of 34 searches (it filled
		// 7 of 14 with "when the query is not in English"), and so find the English books (12 of 12
		// questions against 6 of 12 without the argument, task 32, 2026-11-08).
		description, _ := properties[queryENArg].(map[string]interface{})["description"].(string)
		assert.Contains(t, description, "Always give it when the query is not in English")
		assert.Equal(t, []string{"query"}, parameters["required"], "the English query is optional: the model may leave it out")
		return
	}
	t.Fatal("search_documents is not among the tool definitions")
}

// embedderLog records every text the search embeds, so a test can tell how many searches ran.
type embedderLog struct {
	mu      sync.Mutex
	queries []string
	inner   llm.MockEmbedder
}

func (e *embedderLog) EmbedTexts(ctx context.Context, texts []string, isQuery bool) ([][]float32, error) {
	e.mu.Lock()
	if isQuery {
		e.queries = append(e.queries, texts...)
	}
	e.mu.Unlock()
	return e.inner.EmbedTexts(ctx, texts, isQuery)
}

func (e *embedderLog) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.queries...)
}

func TestSearchDocumentsSearchesTheEnglishQueryToo(t *testing.T) {
	e := newToolEnv(t)
	log := &embedderLog{}
	reg := NewDefaultToolRegistry(retrieval.NewRetrievalService(e.db, log), e.db, e.svc)
	ctx, _ := e.turn("s", false)
	const bulgarian, english = "Как се формират навиците според Уилям Джеймс?", "How are habits formed according to William James?"

	run := func(args map[string]interface{}) []string {
		before := len(log.seen())
		res, err := reg.ExecuteTool(ctx, "search_documents", args)
		require.NoError(t, err)
		assert.Equal(t, "success", resultMap(t, res)["status"])
		return log.seen()[before:]
	}

	assert.Equal(t, []string{bulgarian}, run(map[string]interface{}{"query": bulgarian}),
		"without query_en one search runs, as before")
	assert.Equal(t, []string{bulgarian, english}, run(map[string]interface{}{"query": bulgarian, "query_en": english}),
		"with query_en the question is searched in both languages")
	assert.Equal(t, []string{bulgarian}, run(map[string]interface{}{"query": bulgarian, "query_en": "  " + bulgarian + " "}),
		"the same text is not searched twice")
	assert.Equal(t, []string{bulgarian}, run(map[string]interface{}{"query": bulgarian, "query_en": "   "}),
		"a blank query_en is ignored")
}

// What the English search found must reach the model: with a limit of two passages the Bulgarian
// search alone returns the passage written like the Bulgarian query and one more, and the passage
// written like the English query only comes in through the second search.
func TestSearchDocumentsMergesWhatTheEnglishQueryFound(t *testing.T) {
	e := newToolEnv(t)
	ctx := context.Background()
	reg := NewDefaultToolRegistry(retrieval.NewRetrievalService(e.db, &llm.MockEmbedder{}), e.db, e.svc)
	const bulgarian, english = "навиците на човека според Джеймс", "habits of a person according to James"

	file, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: e.userID, FileName: "principles.txt", FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)
	texts := []string{bulgarian, english, "рецепта за баница със сирене и кори", "how to build a wooden boat in a garden"}
	vectors, err := (&llm.MockEmbedder{}).EmbedTexts(ctx, texts, false)
	require.NoError(t, err)
	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i, text := range texts {
		arg.FileIds = append(arg.FileIds, file.ID)
		arg.UserIds = append(arg.UserIds, e.userID)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, text)
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(vectors[i]))
		arg.ChunkTypes = append(arg.ChunkTypes, "raw_text")
		arg.Metadatas = append(arg.Metadatas, "{}")
	}
	require.NoError(t, e.db.Queries.CreateDocumentChunksBatch(ctx, arg))

	contents := func(args map[string]interface{}) []string {
		turnCtx, _ := e.turn("s", false)
		res, err := reg.ExecuteTool(turnCtx, "search_documents", args)
		require.NoError(t, err)
		var out []string
		for _, r := range resultMap(t, res)["results"].([]map[string]interface{}) {
			out = append(out, r["content"].(string))
		}
		return out
	}

	alone := contents(map[string]interface{}{"query": bulgarian, "limit": float64(2)})
	require.Len(t, alone, 2)
	assert.Equal(t, bulgarian, alone[0], "the passage written like the query is the first")
	assert.NotContains(t, alone, english, "the Bulgarian search alone does not bring the English passage")

	both := contents(map[string]interface{}{"query": bulgarian, "query_en": english, "limit": float64(2)})
	assert.ElementsMatch(t, []string{bulgarian, english}, both, "the English search adds the passage it found")
}
