package retrieval_test

import (
	"context"
	"math"
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

// questionEmbedder answers every question with the same vector, so the tests
// decide the distance of each chunk to the question themselves.
type questionEmbedder struct{ vec []float32 }

func (e questionEmbedder) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = e.vec
	}
	return out, nil
}

// unit returns a 384 dimensional vector whose cosine distance to the question
// vector (the first axis) is exactly distance.
func unit(distance float64) []float32 {
	v := make([]float32, config.VectorDimension)
	cos := 1 - distance
	v[0] = float32(cos)
	v[1] = float32(math.Sqrt(1 - cos*cos))
	return v
}

type chunkSpec struct {
	text      string
	distance  float64
	chunkType string
	metadata  string
}

// chunkEnv is a user with one finished file and the given chunks, plus a service
// that embeds every question as the first axis.
type chunkEnv struct {
	svc  *retrieval.RetrievalService
	user uuid.UUID
}

func newChunkEnv(t *testing.T, chunks []chunkSpec) *chunkEnv {
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

	file, err := d.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: user.ID, FileName: "book.txt", FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)

	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i, c := range chunks {
		kind := c.chunkType
		if kind == "" {
			kind = "raw_text"
		}
		meta := c.metadata
		if meta == "" {
			meta = "{}"
		}
		arg.FileIds = append(arg.FileIds, file.ID)
		arg.UserIds = append(arg.UserIds, user.ID)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, c.text)
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(unit(c.distance)))
		arg.ChunkTypes = append(arg.ChunkTypes, kind)
		arg.Metadatas = append(arg.Metadatas, meta)
	}
	require.NoError(t, d.Queries.CreateDocumentChunksBatch(ctx, arg))

	question := make([]float32, config.VectorDimension)
	question[0] = 1
	return &chunkEnv{
		svc:  retrieval.NewRetrievalService(d, questionEmbedder{vec: question}),
		user: user.ID,
	}
}

func (e *chunkEnv) found(t *testing.T, question string) []string {
	t.Helper()
	res, err := e.svc.SearchRawChunks(context.Background(), e.user, question, 10)
	require.NoError(t, err)
	var texts []string
	for _, r := range res {
		texts = append(texts, r.Content)
	}
	return texts
}

// A question in one language is farther from the passages of a book in another
// language than from passages in its own, for the same quality of match. The
// distance limit is therefore wider (by 0.03) for a chunk in the other language,
// and only for that one.
func TestSearchRawChunksWidensTheDistanceLimitAcrossLanguages(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Български откъс на 0.19", distance: 0.19},
		{text: "Български откъс на 0.21", distance: 0.21},
		{text: "English passage at 0.19", distance: 0.19},
		{text: "English passage at 0.21", distance: 0.21},
		{text: "English passage at 0.25", distance: 0.25},
	})

	// A Bulgarian question: Bulgarian chunks up to 0.20, English ones up to 0.23.
	bulgarian := e.found(t, "зюзюфлекс")
	assert.ElementsMatch(t, []string{"Български откъс на 0.19", "English passage at 0.19", "English passage at 0.21"}, bulgarian)

	// An English question: the other way round.
	english := e.found(t, "zyzyflex")
	assert.ElementsMatch(t, []string{"English passage at 0.19", "Български откъс на 0.19", "Български откъс на 0.21"}, english)
}

// The concept summaries closest to a question carry themes, and the themes widen
// the text half of the search. That finds the passage of an English book that is
// too far from a Bulgarian question for the vector half and shares none of its
// words, which is the case the associative search exists for.
func TestSearchRawChunksFindsPassagesThroughConceptThemes(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{
			text: "Summary of the book on psychic energy.", distance: 0.10, chunkType: "concept_summary",
			metadata: `{"themes": ["libido energy"]}`,
		},
		{text: "The libido is psychic energy that can change its object.", distance: 0.90},
		{text: "An unrelated passage about the weather.", distance: 0.90},
	})

	got := e.found(t, "зюзюфлекс")

	assert.Equal(t, []string{"The libido is psychic energy that can change its object."}, got)
}

// Without a concept summary near the question there are no themes, and a passage
// that is too far away stays out.
func TestSearchRawChunksWithoutNearbyConceptsAddsNothing(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Summary of the book on psychic energy.", distance: 0.60, chunkType: "concept_summary", metadata: `{"themes": ["libido energy"]}`},
		{text: "The libido is psychic energy that can change its object.", distance: 0.90},
	})

	assert.Empty(t, e.found(t, "зюзюфлекс"))
}

func (e *chunkEnv) contextSummaries(t *testing.T, question string) []string {
	t.Helper()
	_, concepts, err := e.svc.SearchForContext(context.Background(), e.user, question, 5)
	require.NoError(t, err)
	var texts []string
	for _, c := range concepts {
		texts = append(texts, c.Content)
	}
	return texts
}

// The summaries that go into the chat prompt must be close: the model weaves them
// into its answer, so an unrelated one - which the books' hub summaries are for any
// short remark, at 0.17-0.21 - does harm. 56 files measured on 2026-11-06.
func TestSearchForContextKeepsOnlyCloseConceptSummaries(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Summary at 0.15", distance: 0.15, chunkType: "concept_summary"},
		{text: "Summary at 0.20", distance: 0.20, chunkType: "concept_summary"},
		{text: "Summary at 0.30", distance: 0.30, chunkType: "concept_summary"},
	})

	assert.Equal(t, []string{"Summary at 0.15"}, e.contextSummaries(t, "зюзюфлекс"))
}

// The tool search takes the themes of summaries up to the wider limit: that is what
// finds an English book from a Bulgarian question (9 of 11 against 6 of 11 without),
// and it must not shrink with the limit of the prompt.
func TestSearchRawChunksTakesThemesFromSummariesBeyondTheContextLimit(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{
			text: "Summary of the book on psychic energy.", distance: 0.20, chunkType: "concept_summary",
			metadata: `{"themes": ["libido energy"]}`,
		},
		{text: "The libido is psychic energy that can change its object.", distance: 0.90},
	})

	assert.Equal(t, []string{"The libido is psychic energy that can change its object."}, e.found(t, "зюзюфлекс"))
	assert.Empty(t, e.contextSummaries(t, "зюзюфлекс"))
}
