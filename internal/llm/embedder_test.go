package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

func constVector(x float32) []float32 {
	v := make([]float32, config.VectorDimension)
	for i := range v {
		v[i] = x
	}
	return v
}

// fakeEmbedderService answers like services/embedder/main.py. Every text gets
// the vector constVector(value).
type fakeEmbedderService struct {
	*httptest.Server
	model   string
	dim     int
	value   float32
	embeds  atomic.Int32 // calls to /embed
	status  int          // /health status, 200 when 0
	vectors int          // how many vectors to answer with, len(texts) when 0
	// valueFor, when set, gives the value of the vector of each text (the text as
	// received, with its "passage: " prefix) instead of value.
	valueFor func(text string) float32
	// vectorLen, when set, is the length of the vectors sent, whatever dim says.
	vectorLen int
	// lastTexts are the texts of the latest /embed request.
	lastTexts []string
}

func newFakeEmbedderService(t *testing.T) *fakeEmbedderService {
	t.Helper()
	f := &fakeEmbedderService{model: config.DefaultEmbedModel, dim: config.VectorDimension, value: 0.5}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if f.status != 0 {
			http.Error(w, "loading", f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "model": f.model, "dim": f.dim})
	})
	mux.HandleFunc("/embed", func(w http.ResponseWriter, r *http.Request) {
		f.embeds.Add(1)
		var req EmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastTexts = req.Texts
		n := len(req.Texts)
		if f.vectors != 0 {
			n = f.vectors
		}
		vectors := make([][]float32, n)
		for i := range vectors {
			value := f.value
			if f.valueFor != nil && i < len(req.Texts) {
				value = f.valueFor(req.Texts[i])
			}
			vectors[i] = constVector(value)
			if f.vectorLen != 0 {
				vectors[i] = vectors[i][:f.vectorLen]
			}
		}
		_ = json.NewEncoder(w).Encode(EmbedResponse{Model: f.model, Dim: f.dim, Vectors: vectors})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// connectTestDB opens the disposable test database with every migration applied.
// db.RunMigrations resolves "migrations" relative to the working directory, so
// the test moves to the module root first.
func connectTestDB(t *testing.T) *db.Database {
	t.Helper()
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, db.RunMigrations(connString))
	database, err := db.Connect(context.Background(), connString)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	return database
}

func newTestEmbedder(t *testing.T, f *fakeEmbedderService) (*HTTPEmbedder, *db.Database) {
	t.Helper()
	database := connectTestDB(t)
	t.Setenv("EMBEDDER_URL", f.URL)
	return NewHTTPEmbedder(database), database
}

func TestCacheKeyDependsOnTheModel(t *testing.T) {
	text := "passage: Здравей"
	assert.Equal(t, cacheKey("model-a", text), cacheKey("model-a", text))
	assert.NotEqual(t, cacheKey("model-a", text), cacheKey("model-b", text),
		"the same text under another model must not find the first one's vector")
	assert.NotEqual(t, cacheKey("model-a", text), hashContent(text),
		"the old key (the text alone) must no longer be used")
}

func TestCheckEmbedResponse(t *testing.T) {
	good := EmbedResponse{Model: config.DefaultEmbedModel, Dim: config.VectorDimension}
	assert.NoError(t, checkEmbedResponse(good))

	// The model that was running by mistake until 2026-11-01.
	err := checkEmbedResponse(EmbedResponse{Model: "paraphrase-multilingual-MiniLM-L12-v2", Dim: 384})
	require.ErrorIs(t, err, ErrEmbedderModel)
	assert.Contains(t, err.Error(), "paraphrase-multilingual-MiniLM-L12-v2")

	require.ErrorIs(t, checkEmbedResponse(EmbedResponse{Model: config.DefaultEmbedModel, Dim: 768}), ErrEmbedderModel)
}

func TestCheckModel(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, _ := newTestEmbedder(t, f)
	ctx := context.Background()

	require.NoError(t, e.CheckModel(ctx))

	f.model = "paraphrase-multilingual-MiniLM-L12-v2"
	require.ErrorIs(t, e.CheckModel(ctx), ErrEmbedderModel)

	f.model = config.DefaultEmbedModel
	f.status = http.StatusServiceUnavailable // the model is still loading
	err := e.CheckModel(ctx)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrEmbedderModel), "an embedder that is not up yet is not a wrong model")
}

func TestEmbedTextsUsesTheCacheOnlyForTheSameModel(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, database := newTestEmbedder(t, f)
	ctx := context.Background()
	text := "Дунав е река " + uuid.NewString() // unique per run: the cache table outlives the test

	// 1. The first call goes to the service and fills the cache.
	got, err := e.EmbedTexts(ctx, []string{text}, false)
	require.NoError(t, err)
	assert.Equal(t, constVector(0.5), got[0])
	assert.EqualValues(t, 1, f.embeds.Load())

	// 2. The second is answered from the cache.
	f.value = 0.9
	got, err = e.EmbedTexts(ctx, []string{text}, false)
	require.NoError(t, err)
	assert.Equal(t, constVector(0.5), got[0], "the cached vector, not the service's new one")
	assert.EqualValues(t, 1, f.embeds.Load(), "no second call to the service")

	// 3. An entry stored under the old key (the text alone), as every vector in
	// the cache was until now, is a vector of an unknown model: it must not be used.
	legacyBody := "legacy " + uuid.NewString()
	legacyText := "passage: " + legacyBody
	_, err = database.Queries.CreateEmbeddingCache(ctx, db.CreateEmbeddingCacheParams{
		ContentHash: hashContent(legacyText),
		Embedding:   pgvector.NewVector(constVector(0.1)),
	})
	require.NoError(t, err)
	got, err = e.EmbedTexts(ctx, []string{legacyBody}, false)
	require.NoError(t, err)
	assert.Equal(t, constVector(0.9), got[0], "the old entry was ignored and the service asked")
	assert.EqualValues(t, 2, f.embeds.Load())
}

func TestEmbedTextsRefusesAWrongModelAndCachesNothing(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, database := newTestEmbedder(t, f)
	ctx := context.Background()
	text := "нещо за вграждане " + uuid.NewString()

	f.model = "paraphrase-multilingual-MiniLM-L12-v2"
	_, err := e.EmbedTexts(ctx, []string{text}, false)
	require.ErrorIs(t, err, ErrEmbedderModel)

	// Neither key may hold the refused vector.
	for _, key := range []string{cacheKey(config.DefaultEmbedModel, "passage: "+text), cacheKey(f.model, "passage: "+text), hashContent("passage: " + text)} {
		_, err := database.Queries.GetEmbeddingCacheByHash(ctx, key)
		assert.Error(t, err, "a vector from the wrong model was cached")
	}

	// The right model is accepted again once the service is fixed.
	f.model = config.DefaultEmbedModel
	got, err := e.EmbedTexts(ctx, []string{text}, false)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestEmbedTextsRefusesWrongDimension(t *testing.T) {
	f := newFakeEmbedderService(t)
	f.dim = 768
	e, _ := newTestEmbedder(t, f)

	_, err := e.EmbedTexts(context.Background(), []string{"текст " + uuid.NewString()}, false)
	require.ErrorIs(t, err, ErrEmbedderModel)
}
