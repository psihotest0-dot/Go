package llm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
)

// countingDB wraps the connection of the Queries and records every statement
// that touches embedding_cache, so a test can say how many round trips a call made.
type countingDB struct {
	db.DBTX

	mu         sync.Mutex
	statements []string
	failReads  bool // makes every SELECT on embedding_cache fail
}

func (c *countingDB) record(query string) {
	if strings.Contains(query, "embedding_cache") {
		c.mu.Lock()
		c.statements = append(c.statements, strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query), "-- name:"))))
		c.mu.Unlock()
	}
}

func (c *countingDB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	c.record(query)
	if c.failReads && strings.Contains(query, "embedding_cache") {
		return nil, errors.New("cache is down")
	}
	return c.DBTX.QueryContext(ctx, query, args...)
}

func (c *countingDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	c.record(query)
	return c.DBTX.ExecContext(ctx, query, args...)
}

// count says how many recorded statements are the sqlc query called name.
func (c *countingDB) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.statements {
		if strings.HasPrefix(s, strings.ToUpper(name)) {
			n++
		}
	}
	return n
}

func (c *countingDB) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = nil
}

// newCountingEmbedder is an embedder whose cache statements are counted.
func newCountingEmbedder(t *testing.T, f *fakeEmbedderService) (*HTTPEmbedder, *countingDB, *db.Database) {
	t.Helper()
	database := connectTestDB(t)

	counter := &countingDB{DBTX: database.SqlDB}
	counted := &db.Database{Pool: database.Pool, SqlDB: database.SqlDB, Queries: db.New(counter)}
	t.Setenv("EMBEDDER_URL", f.URL)
	return NewHTTPEmbedder(counted), counter, database
}

func uniqueTexts(n int) []string {
	run := uuid.NewString()
	texts := make([]string, n)
	for i := range texts {
		texts[i] = fmt.Sprintf("текст номер %d от пуск %s", i, run)
	}
	return texts
}

func TestEmbedTextsReadsTheCacheWithOneQueryPerBatch(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, counter, _ := newCountingEmbedder(t, f)
	ctx := context.Background()
	texts := uniqueTexts(50)

	// First call: every text is new.
	_, err := e.EmbedTexts(ctx, texts, false)
	require.NoError(t, err)
	assert.Equal(t, 1, counter.count("GetEmbeddingCacheByHashes"), "one cache read for 50 texts, not 50")
	assert.Equal(t, 1, counter.count("CreateEmbeddingCacheBatch"), "one cache write for the 50 new vectors")
	assert.EqualValues(t, 1, f.embeds.Load())
	assert.Len(t, f.lastTexts, 50)

	// Second call: all of them are cached.
	counter.reset()
	got, err := e.EmbedTexts(ctx, texts, false)
	require.NoError(t, err)
	assert.Len(t, got, 50)
	assert.Equal(t, 1, counter.count("GetEmbeddingCacheByHashes"), "one cache read for 50 cached texts")
	assert.Zero(t, counter.count("CreateEmbeddingCacheBatch"))
	assert.EqualValues(t, 1, f.embeds.Load(), "no second call to the service")
}

func TestEmbedTextsMixesCachedAndNewKeepingTheOrder(t *testing.T) {
	f := newFakeEmbedderService(t)
	f.valueFor = func(text string) float32 { return float32(len(text)%90) / 100 }
	e, _, _ := newCountingEmbedder(t, f)
	ctx := context.Background()
	texts := uniqueTexts(5)

	// Warm the cache with the 2nd and the 4th text.
	_, err := e.EmbedTexts(ctx, []string{texts[1], texts[3]}, false)
	require.NoError(t, err)
	f.valueFor = func(string) float32 { return 0.99 } // from now on the service answers differently

	got, err := e.EmbedTexts(ctx, texts, false)
	require.NoError(t, err)

	require.Len(t, got, 5)
	for i, text := range texts {
		cached := i == 1 || i == 3
		if cached {
			assert.Equal(t, constVector(float32(len("passage: "+text)%90)/100), got[i], "text %d comes from the cache", i)
		} else {
			assert.Equal(t, constVector(0.99), got[i], "text %d comes from the service", i)
		}
	}
	assert.Len(t, f.lastTexts, 3, "only the three new texts were sent")
	assert.Equal(t, "passage: "+texts[0], f.lastTexts[0], "in the order they were asked")
}

func TestEmbedTextsEmbedsARepeatedTextOnce(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, _, _ := newCountingEmbedder(t, f)
	texts := uniqueTexts(2)
	a, b := texts[0], texts[1]

	got, err := e.EmbedTexts(context.Background(), []string{a, b, a, a}, false)
	require.NoError(t, err)

	require.Len(t, got, 4)
	assert.Len(t, f.lastTexts, 2, "the same text is embedded once")
	assert.Equal(t, got[0], got[2])
	assert.Equal(t, got[0], got[3])
}

func TestEmbedTextsSurvivesAFailingCache(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, counter, _ := newCountingEmbedder(t, f)
	counter.failReads = true

	got, err := e.EmbedTexts(context.Background(), uniqueTexts(3), false)

	require.NoError(t, err, "a broken cache is a slower answer, not a failed one")
	assert.Len(t, got, 3)
	assert.EqualValues(t, 1, f.embeds.Load())
}

func TestEmbedTextsSurvivesAFailingCacheWrite(t *testing.T) {
	f := newFakeEmbedderService(t)
	f.vectorLen = 3 // the service claims 384 dimensions but sends 3 numbers: the cache refuses them
	e, _, database := newCountingEmbedder(t, f)
	texts := uniqueTexts(2)

	got, err := e.EmbedTexts(context.Background(), texts, false)

	require.NoError(t, err)
	assert.Len(t, got, 2)
	rows, err := database.Queries.GetEmbeddingCacheByHashes(context.Background(),
		[]string{cacheKey(config.DefaultEmbedModel, "passage: "+texts[0])})
	require.NoError(t, err)
	assert.Empty(t, rows, "nothing wrong was cached")
}

func TestCacheWriteKeepsTheEntryThatIsThereAlready(t *testing.T) {
	_, _, database := newCountingEmbedder(t, newFakeEmbedderService(t))
	ctx := context.Background()
	key := cacheKey(config.DefaultEmbedModel, "passage: "+uuid.NewString())

	write := func(v float32) error {
		return database.Queries.CreateEmbeddingCacheBatch(ctx, db.CreateEmbeddingCacheBatchParams{
			Hashes:     []string{key, key},
			Embeddings: []string{pgvector.NewVector(constVector(v)).String(), pgvector.NewVector(constVector(v)).String()},
		})
	}
	require.NoError(t, write(0.25), "the same key twice in one statement")
	require.NoError(t, write(0.75), "a concurrent writer that came second")

	rows, err := database.Queries.GetEmbeddingCacheByHashes(ctx, []string{key})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, constVector(0.25), rows[0].Embedding.Slice(), "the first vector stays")
}

func lastAccessed(t *testing.T, database *db.Database, key string) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, database.Pool.QueryRow(context.Background(),
		"SELECT last_accessed_at FROM embedding_cache WHERE content_hash = $1", key).Scan(&at))
	return at
}

func setLastAccessed(t *testing.T, database *db.Database, key string, age time.Duration) {
	t.Helper()
	_, err := database.Pool.Exec(context.Background(),
		"UPDATE embedding_cache SET last_accessed_at = $2 WHERE content_hash = $1", key, time.Now().Add(-age))
	require.NoError(t, err)
}

func TestCacheHitRenewsTheLastUseOnlyOncePerDay(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, _, database := newCountingEmbedder(t, f)
	ctx := context.Background()
	texts := uniqueTexts(2)
	_, err := e.EmbedTexts(ctx, texts, false)
	require.NoError(t, err)

	staleKey := cacheKey(config.DefaultEmbedModel, "passage: "+texts[0])
	freshKey := cacheKey(config.DefaultEmbedModel, "passage: "+texts[1])
	setLastAccessed(t, database, staleKey, 3*24*time.Hour)
	setLastAccessed(t, database, freshKey, time.Hour)
	freshBefore := lastAccessed(t, database, freshKey)

	_, err = e.EmbedTexts(ctx, texts, false)
	require.NoError(t, err)

	assert.WithinDuration(t, time.Now(), lastAccessed(t, database, staleKey), time.Minute, "an entry unused for days is renewed")
	assert.True(t, freshBefore.Equal(lastAccessed(t, database, freshKey)), "an entry used an hour ago is not written again")
}

func TestCacheCleaningGoesByLastUseNotByCreation(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, _, database := newCountingEmbedder(t, f)
	ctx := context.Background()
	texts := uniqueTexts(2)
	_, err := e.EmbedTexts(ctx, texts, false)
	require.NoError(t, err)
	unusedKey := cacheKey(config.DefaultEmbedModel, "passage: "+texts[0])
	usedKey := cacheKey(config.DefaultEmbedModel, "passage: "+texts[1])

	// Both were created 60 days ago; only one was used since.
	_, err = database.Pool.Exec(ctx, "UPDATE embedding_cache SET created_at = now() - interval '60 days' WHERE content_hash = ANY($1)",
		[]string{unusedKey, usedKey})
	require.NoError(t, err)
	setLastAccessed(t, database, unusedKey, 45*24*time.Hour)
	setLastAccessed(t, database, usedKey, 2*24*time.Hour)

	for {
		deleted, err := database.Queries.DeleteOldEmbeddingCache(ctx)
		require.NoError(t, err)
		if deleted == 0 {
			break
		}
	}

	rows, err := database.Queries.GetEmbeddingCacheByHashes(ctx, []string{unusedKey, usedKey})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, usedKey, rows[0].ContentHash, "the vector that is still in use survives its age")
}

// The measurement of 2026-11-08 (plan item P14) showed that a cached vector is
// almost never used again after its first day, so the cache keeps an entry for a
// week after its last use, not a month. The test pins the line between 6 and 8 days.
func TestCacheKeepsAnEntryForAWeekAfterItsLastUse(t *testing.T) {
	f := newFakeEmbedderService(t)
	e, _, database := newCountingEmbedder(t, f)
	ctx := context.Background()
	texts := uniqueTexts(3)
	_, err := e.EmbedTexts(ctx, texts, false)
	require.NoError(t, err)
	keyOf := func(i int) string { return cacheKey(config.DefaultEmbedModel, "passage: "+texts[i]) }

	setLastAccessed(t, database, keyOf(0), 6*24*time.Hour)  // inside the week
	setLastAccessed(t, database, keyOf(1), 8*24*time.Hour)  // just outside it
	setLastAccessed(t, database, keyOf(2), 20*24*time.Hour) // would have stayed under the old 30-day rule

	for {
		deleted, err := database.Queries.DeleteOldEmbeddingCache(ctx)
		require.NoError(t, err)
		if deleted == 0 {
			break
		}
	}

	rows, err := database.Queries.GetEmbeddingCacheByHashes(ctx, []string{keyOf(0), keyOf(1), keyOf(2)})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, keyOf(0), rows[0].ContentHash, "only the entry used within the last week is kept")
}

func TestEmbedTextsTimeoutIsConfigurable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
	}))
	t.Cleanup(slow.Close)

	database := connectTestDB(t)
	t.Setenv("EMBEDDER_URL", slow.URL)

	e := NewHTTPEmbedder(database)
	assert.Equal(t, config.DefaultEmbedderTimeout, e.timeout, "30 s unless configured")
	e.WithTimeout(0)
	assert.Equal(t, config.DefaultEmbedderTimeout, e.timeout, "a zero limit is ignored, not 'no time at all'")

	e.WithTimeout(50 * time.Millisecond)
	start := time.Now()
	_, err := e.EmbedTexts(context.Background(), uniqueTexts(1), false)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 350*time.Millisecond, "the call stopped at the configured limit")
}

func TestNewHTTPEmbedderDoesNotWaitForTheService(t *testing.T) {
	database := connectTestDB(t)
	t.Setenv("EMBEDDER_URL", "http://127.0.0.1:1") // nothing listens there

	start := time.Now()
	e := NewHTTPEmbedder(database)

	assert.NotNil(t, e)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the startup used to wait up to 3 s for a service that is down")
	assert.Error(t, e.CheckModel(context.Background()), "the problem is reported by CheckModel instead")
}
