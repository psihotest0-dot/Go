package retrieval_test

import (
	"context"
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

// scopeEnv is a user with two finished files: "named.pdf", whose chunks are at the given
// distances from the question, and "other.pdf" with six chunks that are nearer to the
// question than any of them. The question is the first axis (see questionEmbedder).
type scopeEnv struct {
	svc   *retrieval.RetrievalService
	d     *db.Database
	user  uuid.UUID
	named uuid.UUID
	other uuid.UUID
}

func (e *scopeEnv) addFile(t *testing.T, name, status string) uuid.UUID {
	t.Helper()
	f, err := e.d.Queries.CreateIngestedFile(context.Background(), db.CreateIngestedFileParams{
		UserID: e.user, FileName: name, FileHash: "hash-" + uuid.NewString(),
		MimeType: "application/pdf", FileSize: 1, Status: status, LocalPath: "storage/none",
	})
	require.NoError(t, err)
	return f.ID
}

func (e *scopeEnv) addChunks(t *testing.T, file uuid.UUID, kind string, specs ...chunkSpec) {
	t.Helper()
	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i, c := range specs {
		arg.FileIds = append(arg.FileIds, file)
		arg.UserIds = append(arg.UserIds, e.user)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, c.text)
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(unit(c.distance)))
		arg.ChunkTypes = append(arg.ChunkTypes, kind)
		arg.Metadatas = append(arg.Metadatas, "{}")
	}
	require.NoError(t, e.d.Queries.CreateDocumentChunksBatch(context.Background(), arg))
}

func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	connString := dbtest.ConnString(t)
	t.Chdir("../..") // db.RunMigrations resolves "migrations" from the working directory
	require.NoError(t, db.RunMigrations(connString))

	ctx := context.Background()
	d, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(d.Close)
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, d.Pool, user.ID) })

	question := make([]float32, config.VectorDimension)
	question[0] = 1
	e := &scopeEnv{d: d, user: user.ID, svc: retrieval.NewRetrievalService(d, questionEmbedder{vec: question})}

	e.named = e.addFile(t, "named.pdf", "completed")
	// Five chunks of the named file; the far ones are beyond the distance limit of the search of the
	// whole library (0.20) and the farthest holds a word that only the text half can find.
	e.addChunks(t, e.named, "raw_text",
		chunkSpec{text: "named near one", distance: 0.05},
		chunkSpec{text: "named near two", distance: 0.07},
		chunkSpec{text: "named middle", distance: 0.12},
		chunkSpec{text: "named far", distance: 0.45},
		chunkSpec{text: "named farthest зюзюфлекс", distance: 0.60},
	)
	e.addChunks(t, e.named, "concept_summary", chunkSpec{text: "named concept summary", distance: 0.01})

	e.other = e.addFile(t, "other.pdf", "completed")
	e.addChunks(t, e.other, "raw_text",
		chunkSpec{text: "other one", distance: 0.01}, chunkSpec{text: "other two", distance: 0.02},
		chunkSpec{text: "other three", distance: 0.03}, chunkSpec{text: "other four", distance: 0.04},
		chunkSpec{text: "other five", distance: 0.045}, chunkSpec{text: "other six", distance: 0.046},
	)
	return e
}

func contents(res []retrieval.SearchResult) []string {
	var out []string
	for _, r := range res {
		out = append(out, r.Content)
	}
	return out
}

// The search of the whole library keeps two passages of a file and fills the answer with the
// nearest ones of other books (2026-11-15: a question about CLBSV2.pdf showed 2 passages of it
// and 6 of Freud, a cookbook and a book about vitamin B12). Inside the file nothing is capped
// or cut by distance, and nothing from another file comes in.
func TestSearchFileChunksReturnsTheWholeFileAndNothingElse(t *testing.T) {
	e := newScopeEnv(t)
	ctx := context.Background()

	// What the scope changes: the search of the whole library cuts the chunks beyond the distance
	// limit and mixes in the other book.
	whole, err := e.svc.SearchRawChunks(ctx, e.user, "какво пише във файла", 8)
	require.NoError(t, err)
	assert.NotContains(t, contents(whole), "named far", "beyond the distance limit of the whole library")
	assert.Contains(t, contents(whole), "other one", "the other book comes in")

	res, err := e.svc.SearchFileChunks(ctx, e.user, e.named, "какво пише във файла", 8)
	require.NoError(t, err)
	assert.Equal(t, []string{"named near one", "named near two", "named middle", "named far", "named farthest зюзюфлекс"}, contents(res),
		"all five raw chunks, nearest first, however far; no concept summary, no other file")
	for _, r := range res {
		assert.Equal(t, "named.pdf", r.FileName)
		assert.Equal(t, "chunk", r.SourceType)
	}

	res, err = e.svc.SearchFileChunks(ctx, e.user, e.named, "какво пише във файла", 3)
	require.NoError(t, err)
	assert.Equal(t, []string{"named near one", "named near two", "named middle"}, contents(res), "the limit")
}

func TestSearchFileChunksTextHalfFindsTheWordInAFarChunk(t *testing.T) {
	e := newScopeEnv(t)

	res, err := e.svc.SearchFileChunks(context.Background(), e.user, e.named, "къде е зюзюфлекс", 3)
	require.NoError(t, err)
	require.NotEmpty(t, res)
	assert.Equal(t, "named farthest зюзюфлекс", res[0].Content, "the chunk with the word comes first although it is the farthest")
	assert.Len(t, res, 3)
}

func TestSearchFileChunksIsScopedToTheUser(t *testing.T) {
	e := newScopeEnv(t)

	res, err := e.svc.SearchFileChunks(context.Background(), uuid.New(), e.named, "какво пише във файла", 8)
	require.NoError(t, err)
	assert.Empty(t, res, "another user cannot read the file by its id")
}

func TestSearchFileChunksOfAFileThatIsNotFinishedOrWasDeletedFindsNothing(t *testing.T) {
	e := newScopeEnv(t)
	ctx := context.Background()
	pending := e.addFile(t, "pending.pdf", "processing")
	e.addChunks(t, pending, "raw_text", chunkSpec{text: "pending chunk", distance: 0.05})
	gone := e.addFile(t, "gone.pdf", "completed")
	e.addChunks(t, gone, "raw_text", chunkSpec{text: "gone chunk", distance: 0.05})
	require.NoError(t, e.d.Queries.SoftDeleteIngestedFile(ctx, db.SoftDeleteIngestedFileParams{ID: gone, UserID: e.user}))

	for _, id := range []uuid.UUID{pending, gone} {
		res, err := e.svc.SearchFileChunks(ctx, e.user, id, "какво пише във файла", 8)
		require.NoError(t, err)
		assert.Empty(t, res)
	}
}

// FileNamedIn reads the list of the user's own files: a finished one is named, a file that is
// still being processed, deleted or somebody else's is not.
func TestFileNamedInKnowsOnlyTheUsersFinishedFiles(t *testing.T) {
	e := newScopeEnv(t)
	ctx := context.Background()
	e.addFile(t, "pending-notes.pdf", "processing")
	gone := e.addFile(t, "gone-notes.pdf", "completed")
	require.NoError(t, e.d.Queries.SoftDeleteIngestedFile(ctx, db.SoftDeleteIngestedFileParams{ID: gone, UserID: e.user}))

	got, ok := e.svc.FileNamedIn(ctx, e.user, "какво пише във файла named.pdf")
	require.True(t, ok)
	assert.Equal(t, "named.pdf", got.Name)
	assert.Equal(t, e.named, got.ID)

	for _, text := range []string{"какво пише във файла pending-notes.pdf", "какво пише във файла gone-notes.pdf"} {
		_, ok := e.svc.FileNamedIn(ctx, e.user, text)
		assert.False(t, ok, text)
	}
	_, ok = e.svc.FileNamedIn(ctx, uuid.New(), "какво пише във файла named.pdf")
	assert.False(t, ok, "another user has no such file")
}
