package reembed

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

const oldModel = "paraphrase-multilingual-MiniLM-L12-v2"

// fakeEmbedder returns constVector(value) for every text and remembers what it
// was asked to embed.
type fakeEmbedder struct {
	mu     sync.Mutex
	value  float32
	texts  []string
	badLen bool // answer with one vector too few
	badDim bool // answer with vectors of the wrong size
}

func constVector(x float32, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = x
	}
	return v
}

func (f *fakeEmbedder) EmbedTexts(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, texts...)
	n := len(texts)
	if f.badLen {
		n--
	}
	dim := config.VectorDimension
	if f.badDim {
		dim = 3
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = constVector(f.value, dim)
	}
	return out, nil
}

type env struct {
	ctx  context.Context
	d    *db.Database
	user db.User
	file db.IngestedFile
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	d, err := db.Connect(ctx, dbtest.ConnString(t))
	require.NoError(t, err)
	t.Cleanup(d.Close)
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, d.Pool, user.ID) })
	file, err := d.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: user.ID, FileName: "книга.txt", FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "x",
	})
	require.NoError(t, err)
	return &env{ctx: ctx, d: d, user: user, file: file}
}

// chunk stores a chunk whose vector is constVector(0.1) and whose recorded model
// is model (nil = not known).
func (e *env) chunk(t *testing.T, index int, chunkType, text string, model *string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, e.d.SqlDB.QueryRowContext(e.ctx,
		`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata, embedding_model)
		 VALUES ($1, $2, $3, $4, $5, $6, '{}', $7) RETURNING id`,
		e.file.ID, e.user.ID, index, text, pgvector.NewVector(constVector(0.1, config.VectorDimension)), chunkType, model).Scan(&id))
	return id
}

func (e *env) chunkState(t *testing.T, id uuid.UUID) (first float32, model *string) {
	t.Helper()
	var v pgvector.Vector
	require.NoError(t, e.d.SqlDB.QueryRowContext(e.ctx, `SELECT embedding, embedding_model FROM document_chunks WHERE id = $1`, id).Scan(&v, &model))
	return v.Slice()[0], model
}

func strPtr(s string) *string { return &s }

func TestChunksRecomputesWhatIsNotMadeByTheCurrentModel(t *testing.T) {
	e := newEnv(t)
	unknown := e.chunk(t, 0, "raw_text", "първи откъс", nil)
	other := e.chunk(t, 1, "raw_text", "втори откъс", strPtr(oldModel))
	current := e.chunk(t, 2, "raw_text", "трети откъс", strPtr(config.DefaultEmbedModel))
	concept := e.chunk(t, 3, "concept_summary", "обобщение", nil)

	emb := &fakeEmbedder{value: 0.7}
	n, err := Chunks(e.ctx, e.d, emb, Options{BatchSize: 2}) // 3 rows, 2 per batch: more than one round
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	for _, id := range []uuid.UUID{unknown, other, concept} {
		v, model := e.chunkState(t, id)
		assert.InDelta(t, 0.7, v, 1e-6, "vector was not replaced")
		require.NotNil(t, model)
		assert.Equal(t, config.DefaultEmbedModel, *model)
	}
	v, _ := e.chunkState(t, current)
	assert.InDelta(t, 0.1, v, 1e-6, "a chunk of the current model must be left alone")

	// What was embedded is what ingest and distillation embed.
	assert.ElementsMatch(t, []string{
		"[Файл: книга.txt - Chunk 0] първи откъс",
		"[Файл: книга.txt - Chunk 1] втори откъс",
		"обобщение",
	}, emb.texts)

	// A second run has nothing left to do.
	emb.texts = nil
	n, err = Chunks(e.ctx, e.d, emb, Options{})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, emb.texts)
}

func TestChunksForceRecomputesEverything(t *testing.T) {
	e := newEnv(t)
	current := e.chunk(t, 0, "raw_text", "откъс", strPtr(config.DefaultEmbedModel))

	n, err := Chunks(e.ctx, e.d, &fakeEmbedder{value: 0.4}, Options{Force: true})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	v, _ := e.chunkState(t, current)
	assert.InDelta(t, 0.4, v, 1e-6)
}

func TestChunksWritesNothingFromABadAnswer(t *testing.T) {
	for name, emb := range map[string]*fakeEmbedder{
		"missing vector":  {value: 0.7, badLen: true},
		"wrong dimension": {value: 0.7, badDim: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			a := e.chunk(t, 0, "raw_text", "а", nil)
			b := e.chunk(t, 1, "raw_text", "б", nil)

			_, err := Chunks(e.ctx, e.d, emb, Options{})
			require.Error(t, err)
			for _, id := range []uuid.UUID{a, b} {
				v, model := e.chunkState(t, id)
				assert.InDelta(t, 0.1, v, 1e-6)
				assert.Nil(t, model, "an unverified row must stay marked as not known")
			}
		})
	}
}

func (e *env) memory(t *testing.T, content, model string) uuid.UUID {
	t.Helper()
	m, err := e.d.Queries.CreateMemory(e.ctx, db.CreateMemoryParams{
		UserID: e.user.ID, Content: content, Status: "active",
		Embedding: pgvector.NewVector(constVector(0.1, config.VectorDimension)), EmbeddingModel: model,
		Importance: 5, Metadata: pqtype.NullRawMessage{}, ContentHash: db.GenerateHash(content + uuid.NewString()),
	})
	require.NoError(t, err)
	return m.ID
}

func (e *env) memoryState(t *testing.T, id uuid.UUID) (first float32, model string) {
	t.Helper()
	var v pgvector.Vector
	require.NoError(t, e.d.SqlDB.QueryRowContext(e.ctx, `SELECT embedding, embedding_model FROM memories WHERE id = $1`, id).Scan(&v, &model))
	return v.Slice()[0], model
}

// The memories written before 2026-11-01 say they were made by E5 while a
// different model made them: only Force sets that right.
func TestMemoriesTrustTheRecordedModelUnlessForced(t *testing.T) {
	e := newEnv(t)
	liar := e.memory(t, "Живея в София", config.DefaultEmbedModel)
	honest := e.memory(t, "Обичам пица", oldModel)

	n, err := Memories(e.ctx, e.d, &fakeEmbedder{value: 0.6}, Options{})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the one that says it is from another model")
	v, model := e.memoryState(t, honest)
	assert.InDelta(t, 0.6, v, 1e-6)
	assert.Equal(t, config.DefaultEmbedModel, model)
	v, _ = e.memoryState(t, liar)
	assert.InDelta(t, 0.1, v, 1e-6, "the recorded model was believed")

	n, err = Memories(e.ctx, e.d, &fakeEmbedder{value: 0.3}, Options{Force: true})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	v, _ = e.memoryState(t, liar)
	assert.InDelta(t, 0.3, v, 1e-6, "Force recomputes it")
}

func TestOptionsDefaults(t *testing.T) {
	assert.EqualValues(t, 64, Options{}.batch())
	assert.EqualValues(t, 5, Options{BatchSize: 5}.batch())
	assert.NotNil(t, Options{}.logger())
}
