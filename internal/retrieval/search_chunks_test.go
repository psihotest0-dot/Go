package retrieval_test

import (
	"context"
	"math"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
	"go-monolith/metrics"
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

// searchObservations is what a series of the search quality metrics holds so far:
// the number of observations and their sum.
type searchObservations struct {
	count uint64
	sum   float64
}

func observed(t *testing.T, h *prometheus.HistogramVec, search string) searchObservations {
	t.Helper()
	var m dto.Metric
	require.NoError(t, h.WithLabelValues(search).(prometheus.Histogram).Write(&m))
	return searchObservations{count: m.GetHistogram().GetSampleCount(), sum: m.GetHistogram().GetSampleSum()}
}

// since is the observations added after before.
func (o searchObservations) since(before searchObservations) searchObservations {
	return searchObservations{count: o.count - before.count, sum: o.sum - before.sum}
}

// The tool search records how many passages it returned and how near the nearest
// one the vector half found is (plan item P30): the distance limits were set by
// hand, and a drift shows only in such a measure. A passage that only the text half
// found has no distance and must not count as the nearest one.
func TestSearchRawChunksMeasuresTheNearestPassage(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Български откъс на 0.15", distance: 0.15},
		{text: "Български откъс на 0.12", distance: 0.12},
		{text: "Откъс, далеч от въпроса, в който пише зюзюфлекс", distance: 0.90},
	})
	distBefore := observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchChunks)
	resultsBefore := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks)
	contextBefore := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchContext)

	require.Len(t, e.found(t, "зюзюфлекс"), 3)

	dist := observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchChunks).since(distBefore)
	assert.Equal(t, uint64(1), dist.count)
	assert.InDelta(t, 0.12, dist.sum, 1e-5)
	results := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks).since(resultsBefore)
	assert.Equal(t, searchObservations{count: 1, sum: 3}, results)
	// The summaries looked up for their themes are not the search of the prompt.
	assert.Equal(t, contextBefore, observed(t, metrics.RetrievalResults, metrics.RetrievalSearchContext))
}

// A search that returned nothing, or only what the text half found, is seen in the
// count of results; the distance has nothing to show for it.
func TestSearchRawChunksWithoutAVectorMatchRecordsNoDistance(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Откъс, далеч от въпроса, в който пише зюзюфлекс", distance: 0.90},
	})
	distBefore := observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchChunks)
	resultsBefore := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks)

	require.Len(t, e.found(t, "зюзюфлекс"), 1)
	assert.Empty(t, e.found(t, "куркумаджия"))

	assert.Equal(t, distBefore, observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchChunks))
	results := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks).since(resultsBefore)
	assert.Equal(t, searchObservations{count: 2, sum: 1}, results)
}

// The search for the summaries of the prompt is measured on its own, after its
// limit of 0.17.
func TestSearchForContextMeasuresTheNearestSummary(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Summary at 0.15", distance: 0.15, chunkType: "concept_summary"},
		{text: "Summary at 0.12", distance: 0.12, chunkType: "concept_summary"},
		{text: "Summary at 0.30", distance: 0.30, chunkType: "concept_summary"},
	})
	distBefore := observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchContext)
	resultsBefore := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchContext)
	chunksBefore := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks)

	require.Len(t, e.contextSummaries(t, "зюзюфлекс"), 2)

	dist := observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchContext).since(distBefore)
	assert.Equal(t, uint64(1), dist.count)
	assert.InDelta(t, 0.12, dist.sum, 1e-5)
	results := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchContext).since(resultsBefore)
	assert.Equal(t, searchObservations{count: 1, sum: 2}, results)
	assert.Equal(t, chunksBefore, observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks))
}

// A prompt search with no summary within the limit - the usual case for a greeting -
// is a 0 in the count and nothing in the distance.
func TestSearchForContextWithNothingCloseRecordsNoDistance(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Summary at 0.20", distance: 0.20, chunkType: "concept_summary"},
	})
	distBefore := observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchContext)
	resultsBefore := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchContext)

	assert.Empty(t, e.contextSummaries(t, "зюзюфлекс"))

	assert.Equal(t, distBefore, observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchContext))
	results := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchContext).since(resultsBefore)
	assert.Equal(t, searchObservations{count: 1, sum: 0}, results)
}

// Without the embedder the tool search falls back to the text half alone: it is
// still a search with a count of results, and it has no distance.
func TestSearchRawChunksTextFallbackRecordsOnlyTheCount(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()
	d := &db.Database{SqlDB: mockDB, Queries: db.New(mockDB)}
	svc := retrieval.NewRetrievalService(d, &llm.MockEmbedderWithConfig{EmptyVectors: true})

	mock.MatchExpectationsInOrder(false)
	expectSearchTx(mock)
	mock.ExpectQuery(`(?i)SELECT c.id, c.text as content, f.file_name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "content", "file_name", "score"}).
			AddRow(uuid.New(), "A chunk", "book.txt", 0.8).
			AddRow(uuid.New(), "Another chunk", "book.txt", 0.7))

	distBefore := observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchChunks)
	resultsBefore := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks)

	res, err := svc.SearchRawChunks(context.Background(), uuid.New(), "зюзюфлекс", 8)
	require.NoError(t, err)
	require.Len(t, res, 2)

	assert.Equal(t, distBefore, observed(t, metrics.RetrievalNearestDistance, metrics.RetrievalSearchChunks))
	results := observed(t, metrics.RetrievalResults, metrics.RetrievalSearchChunks).since(resultsBefore)
	assert.Equal(t, searchObservations{count: 1, sum: 2}, results)
}

// Only the two measured searches have series: the searches that are only a means
// (the themes of the tool search, SearchAll) must not add one with an empty label.
func TestOnlyTheMeasuredSearchesHaveSeries(t *testing.T) {
	e := newChunkEnv(t, []chunkSpec{
		{text: "Summary at 0.15", distance: 0.15, chunkType: "concept_summary", metadata: `{"themes": ["energy"]}`},
		{text: "Български откъс на 0.12", distance: 0.12},
	})
	e.found(t, "зюзюфлекс")
	_, _, err := e.svc.SearchAll(context.Background(), e.user, "зюзюфлекс", 5)
	require.NoError(t, err)

	for _, h := range []*prometheus.HistogramVec{metrics.RetrievalNearestDistance, metrics.RetrievalResults} {
		ch := make(chan prometheus.Metric, 16)
		h.Collect(ch)
		close(ch)
		var searches []string
		for m := range ch {
			var pb dto.Metric
			require.NoError(t, m.Write(&pb))
			for _, l := range pb.GetLabel() {
				searches = append(searches, l.GetValue())
			}
		}
		assert.ElementsMatch(t, []string{metrics.RetrievalSearchChunks, metrics.RetrievalSearchContext}, searches)
	}
}
