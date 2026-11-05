package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

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

// embedOnly is the client of one service without the database: these tests are
// about a single HTTP call, not about the cache.
func embedOnly(url string) *HTTPEmbedder {
	return &HTTPEmbedder{baseURL: url, client: &http.Client{}, timeout: config.DefaultEmbedderTimeout}
}

func answerWithOneVector(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(EmbedResponse{
		Model:   config.DefaultEmbedModel,
		Dim:     config.VectorDimension,
		Vectors: [][]float32{constVector(0.5)},
	})
}

// dropConnection closes the connection without a single byte of answer, which is
// what uvicorn did on 2026-11-04 to a keep-alive connection net/http had just
// taken out of its pool.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

func TestEmbedRemoteRetriesADroppedConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			dropConnection(t, w)
			return
		}
		answerWithOneVector(w)
	}))
	t.Cleanup(srv.Close)

	vectors, err := embedOnly(srv.URL).embedRemote(context.Background(), []string{"passage: текст"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.Equal(t, constVector(0.5), vectors[0])
	assert.EqualValues(t, 2, calls.Load(), "the call that lost its connection was repeated")
}

func TestEmbedRemoteRetriesAServiceThatIsStarting(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "Model not loaded yet", http.StatusServiceUnavailable)
			return
		}
		answerWithOneVector(w)
	}))
	t.Cleanup(srv.Close)

	vectors, err := embedOnly(srv.URL).embedRemote(context.Background(), []string{"passage: текст"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.EqualValues(t, 2, calls.Load(), "the service answered 503 while it was loading its model")
}

func TestEmbedRemoteGivesUpAfterTheLastAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		dropConnection(t, w)
	}))
	t.Cleanup(srv.Close)

	_, err := embedOnly(srv.URL).embedRemote(context.Background(), []string{"passage: текст"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embed request failed after 3 attempts")
	assert.EqualValues(t, embedAttempts, calls.Load())
}

func TestEmbedRemoteDoesNotRetryARefusedRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "Each text must be at most 10000 characters long", http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv.Close)

	_, err := embedOnly(srv.URL).embedRemote(context.Background(), []string{"passage: текст"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "422")
	assert.EqualValues(t, 1, calls.Load(), "a request the service refuses is refused again")
}

func TestEmbedRemoteDoesNotRetryAWrongModel(t *testing.T) {
	f := newFakeEmbedderService(t)
	f.model = "paraphrase-multilingual-MiniLM-L12-v2"

	_, err := embedOnly(f.URL).embedRemote(context.Background(), []string{"passage: текст"})
	require.ErrorIs(t, err, ErrEmbedderModel)
	assert.EqualValues(t, 1, f.embeds.Load(), "the wrong model would answer the same way again")
}

// cutAnswerShort sends the headers of a 200 and the first bytes of the body, then
// closes the connection: the answer stops in the middle, short of its
// Content-Length.
func cutAnswerShort(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 5000\r\n\r\n{\"model\":\"intfl"))
	_ = conn.Close()
}

func TestEmbedRemoteRetriesAnAnswerCutShort(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			cutAnswerShort(t, w)
			return
		}
		answerWithOneVector(w)
	}))
	t.Cleanup(srv.Close)

	vectors, err := embedOnly(srv.URL).embedRemote(context.Background(), []string{"passage: текст"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.EqualValues(t, 2, calls.Load(), "an answer that stopped in the middle was asked for again")
}

func TestEmbedRemoteRetriesAnEmptyAnswer(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusOK) // a 200 with no body at all
			return
		}
		answerWithOneVector(w)
	}))
	t.Cleanup(srv.Close)

	vectors, err := embedOnly(srv.URL).embedRemote(context.Background(), []string{"passage: текст"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.EqualValues(t, 2, calls.Load(), "an empty answer was asked for again")
}

// Only the statuses that say "not now" are repeated; the others say "not this
// request", and the same request gets the same answer.
func TestEmbedRemoteRetriesOnlyTheStatusesThatMayPass(t *testing.T) {
	tests := []struct {
		status  int
		retried bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusRequestEntityTooLarge, false},
		{http.StatusUnprocessableEntity, false},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					http.Error(w, "no", tt.status)
					return
				}
				answerWithOneVector(w)
			}))
			t.Cleanup(srv.Close)

			vectors, err := embedOnly(srv.URL).embedRemote(context.Background(), []string{"passage: текст"})
			if tt.retried {
				require.NoError(t, err)
				require.Len(t, vectors, 1)
				assert.EqualValues(t, 2, calls.Load())
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), strconv.Itoa(tt.status))
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

// A call whose context is done is not worth another attempt: the caller has
// stopped waiting, whatever the error looks like.
func TestEmbedRemoteDoesNotRetryOnceTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cancel()
		dropConnection(t, w)
	}))
	t.Cleanup(srv.Close)

	_, err := embedOnly(srv.URL).embedRemote(ctx, []string{"passage: текст"})
	require.Error(t, err)
	assert.EqualValues(t, 1, calls.Load(), "the caller gave up, so the call was not repeated")
}

// The time limit of EmbedTexts bounds all attempts together, the pause between
// them included: when it runs out during the pause, the next attempt is not made.
func TestEmbedRemoteStopsWaitingWhenTheLimitRunsOutBetweenAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		dropConnection(t, w)
	}))
	t.Cleanup(srv.Close)

	// Shorter than embedRetryDelay, so the limit ends inside the first pause.
	ctx, cancel := context.WithTimeout(context.Background(), embedRetryDelay/2)
	t.Cleanup(cancel)

	start := time.Now()
	_, err := embedOnly(srv.URL).embedRemote(ctx, []string{"passage: текст"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.EqualValues(t, 1, calls.Load(), "no second attempt after the limit")
	assert.Less(t, time.Since(start), embedRetryDelay*4, "the call returned when the limit ran out, not after all attempts")
}
