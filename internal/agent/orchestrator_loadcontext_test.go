package agent

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/retrieval"
)

// questionAtOrigin embeds every question as the first axis, so a test decides the
// distance of each memory and summary to it (see unitAt).
type questionAtOrigin struct{}

func (questionAtOrigin) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, config.VectorDimension)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

// unitAt is a vector whose cosine distance to questionAtOrigin's answer is distance.
func unitAt(distance float64) []float32 {
	v := make([]float32, config.VectorDimension)
	cos := 1 - distance
	v[0] = float32(cos)
	v[1] = float32(math.Sqrt(1 - cos*cos))
	return v
}

type contextEnv struct {
	o      *Orchestrator
	d      *db.Database
	userID uuid.UUID
	fileID uuid.UUID
}

func newContextEnv(t *testing.T) *contextEnv {
	t.Helper()
	connString := dbtest.ConnString(t)
	t.Chdir("../..") // db.RunMigrations resolves "migrations" from the working directory
	ctx := context.Background()
	require.NoError(t, db.RunMigrations(connString))
	d, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(d.Close)

	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, d.Pool, user.ID) })

	file, err := d.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: user.ID, FileName: "book.txt", FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)

	rs := retrieval.NewRetrievalService(d, questionAtOrigin{})
	return &contextEnv{o: &Orchestrator{db: d, retrieval: rs}, d: d, userID: user.ID, fileID: file.ID}
}

func (e *contextEnv) memory(t *testing.T, content string, importance int32, distance float64) {
	t.Helper()
	_, err := e.d.Queries.CreateMemory(context.Background(), db.CreateMemoryParams{
		UserID: e.userID, Content: content, Status: "active",
		Embedding: pgvector.NewVector(unitAt(distance)), EmbeddingModel: config.DefaultEmbedModel,
		Importance: importance, ContentHash: uuid.NewString(),
	})
	require.NoError(t, err)
}

func (e *contextEnv) summaries(t *testing.T, distances map[string]float64) {
	t.Helper()
	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	i := 0
	for text, distance := range distances {
		arg.FileIds = append(arg.FileIds, e.fileID)
		arg.UserIds = append(arg.UserIds, e.userID)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, text)
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(unitAt(distance)))
		arg.ChunkTypes = append(arg.ChunkTypes, "concept_summary")
		arg.Metadatas = append(arg.Metadatas, "{}")
		i++
	}
	require.NoError(t, e.d.Queries.CreateDocumentChunksBatch(context.Background(), arg))
}

func (e *contextEnv) load(t *testing.T) (chatContext, []string) {
	t.Helper()
	chatCtx, memoryIDs, err := e.o.loadContext(context.Background(), e.userID, ChatRequest{SessionID: "session", Content: "зюзюфлекс"})
	require.NoError(t, err)
	return chatCtx, memoryIDs
}

// The prompt carries the profile facts at every message. While the memories are no
// more than the profile holds, "Лична памет" would only repeat them - and the summary
// that is not close stays out, the close one comes with its distance, not a percentage.
func TestLoadContextDoesNotRepeatTheProfileAndShowsDistances(t *testing.T) {
	e := newContextEnv(t)
	e.memory(t, "Ивайло се казва.", 5, 0.10)
	e.memory(t, "малко уморен днес", 5, 0.12)
	e.memory(t, "промени по един друг проект", 5, 0.20)
	e.summaries(t, map[string]float64{"Close summary": 0.15, "Closer summary": 0.12, "Far summary": 0.20})

	chatCtx, memoryIDs := e.load(t)

	assert.ElementsMatch(t, []string{"Ивайло се казва.", "малко уморен днес", "промени по един друг проект"}, chatCtx.TopMemories)
	assert.Empty(t, chatCtx.Memories, "all three are in the profile already")
	assert.Empty(t, memoryIDs)
	assert.Equal(t, []string{"Closer summary", "Close summary"}, chatCtx.Chunks, "the far summary (0.20: inside the tool search's limit of 0.25, outside the prompt's 0.17) stays out")

	require.Len(t, chatCtx.Sources, 2)
	assert.Equal(t, api.SourceInfo{
		ChunkID: chatCtx.Sources[0].ChunkID, FileName: "book.txt", ContentSnippet: "Closer summary",
		Score: 0.12, ScoreType: api.ScoreTypeDistance, SourceType: "concept",
	}, chatCtx.Sources[0], "the closest first, as a distance")
	assert.Equal(t, 0.15, chatCtx.Sources[1].Score)
}

// A question about the library gets no concept summaries, in the prompt or in the
// panel: its answer is the list of files, and the summaries near it are covers and
// introductions that only match the word "книги". The memories stay, and any
// other question still gets its summaries.
func TestLoadContextLeavesTheSummariesOutOfALibraryQuestion(t *testing.T) {
	e := newContextEnv(t)
	for i := 0; i < topMemoryLimit; i++ {
		e.memory(t, "important and far "+uuid.NewString(), 9, 0.90)
	}
	e.memory(t, "unimportant but near", 1, 0.10)
	e.summaries(t, map[string]float64{"The cover of a book": 0.12})
	load := func(question string) chatContext {
		chatCtx, _, err := e.o.loadContext(context.Background(), e.userID, ChatRequest{SessionID: "session", Content: question})
		require.NoError(t, err)
		return chatCtx
	}

	for _, q := range []string{"С какви книги разполагаш?", "What books do you have?", "Имаш ли книга за стоицизма?"} {
		lib := load(q)
		assert.Empty(t, lib.Chunks, q)
		assert.Equal(t, []string{"unimportant but near"}, lib.Memories, q)
		require.Len(t, lib.Sources, 1, q)
		assert.Equal(t, "memory", lib.Sources[0].SourceType, q)
	}

	other := load("Какво казва Марк Аврелий за смъртта?")
	assert.Equal(t, []string{"The cover of a book"}, other.Chunks, "a question that is not about the library keeps its summaries")
}

// Beyond the profile's limit a memory that fits the message is still found and
// shown: as a source with no measure, after the summaries.
func TestLoadContextStillFindsAMemoryOutsideTheProfile(t *testing.T) {
	e := newContextEnv(t)
	for i := 0; i < topMemoryLimit; i++ {
		e.memory(t, "important and far "+uuid.NewString(), 9, 0.90)
	}
	e.memory(t, "unimportant but near", 1, 0.10)
	e.summaries(t, map[string]float64{"Close summary": 0.15})

	chatCtx, memoryIDs := e.load(t)

	assert.Len(t, chatCtx.TopMemories, topMemoryLimit)
	assert.NotContains(t, chatCtx.TopMemories, "unimportant but near")
	assert.Equal(t, []string{"unimportant but near"}, chatCtx.Memories)
	assert.Len(t, memoryIDs, 1)

	require.Len(t, chatCtx.Sources, 2)
	assert.Equal(t, "concept", chatCtx.Sources[0].SourceType)
	assert.Equal(t, "memory", chatCtx.Sources[1].SourceType)
	assert.Zero(t, chatCtx.Sources[1].Score, "a memory has no distance to show")
	assert.Empty(t, chatCtx.Sources[1].ScoreType)
}
