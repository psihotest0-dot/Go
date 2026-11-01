package db_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// unitVec is a vector with a single 1 at position i: its cosine distance to
// itself is 0 and to any other unitVec is 1, which makes "near" and "far" exact.
func unitVec(i int) pgvector.Vector {
	v := make([]float32, config.VectorDimension)
	v[i] = 1
	return pgvector.NewVector(v)
}

type searchEnv struct {
	ctx  context.Context
	d    *db.Database
	user db.User
}

func newSearchEnv(t *testing.T) *searchEnv {
	t.Helper()
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, db.RunMigrations(connString))

	ctx := context.Background()
	d, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(d.Close)
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, d.Pool, user.ID) })
	return &searchEnv{ctx: ctx, d: d, user: user}
}

// file stores a file of the given status and category for the user.
func (e *searchEnv) file(t *testing.T, owner uuid.UUID, name, status, category string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, e.d.SqlDB.QueryRowContext(e.ctx,
		`INSERT INTO ingested_files (user_id, file_name, file_hash, mime_type, file_size, status, local_path, category)
		 VALUES ($1, $2, $3, 'text/plain', 1, $4, 'x', $5) RETURNING id`,
		owner, name, "hash-"+uuid.NewString(), status, category).Scan(&id))
	return id
}

func (e *searchEnv) chunk(t *testing.T, owner, file uuid.UUID, index int, text, chunkType string, vec pgvector.Vector) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, e.d.SqlDB.QueryRowContext(e.ctx,
		`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, '{}') RETURNING id`,
		file, owner, index, text, vec, chunkType).Scan(&id))
	return id
}

func (e *searchEnv) hybrid(t *testing.T, tsQuery string, queryVec pgvector.Vector, matchCount int32) []db.SearchHybridChunksRow {
	t.Helper()
	var rows []db.SearchHybridChunksRow
	require.NoError(t, e.d.WithSearchSettings(e.ctx, func(q *db.Queries) (err error) {
		rows, err = q.SearchHybridChunks(e.ctx, db.SearchHybridChunksParams{
			UserID: e.user.ID, MatchCount: matchCount, Embedding: queryVec,
			VectorLimit: db.SearchVectorCandidates, MaxDistance: 0.45, TsQuery: tsQuery,
		})
		return err
	}))
	return rows
}

const (
	danube  = "Дунав е река на границата между България и Румъния."
	capital = "Столицата на България е София и тя е най-големият град в страната."
)

// similarity() scored "река Дунав граница" against a chunk holding all three
// words at 0.11, below the 0.15 cut-off, so the text half of the hybrid search
// returned nothing. The query vector here is far from every chunk, so only the
// text half can find it. The prefixes stand in for stemming: "дуна:*" finds
// "Дунав" and "грани:*" finds "границата".
func TestHybridSearchFindsAChunkByItsWords(t *testing.T) {
	e := newSearchEnv(t)
	file := e.file(t, e.user.ID, "geo.txt", "completed", "general")
	want := e.chunk(t, e.user.ID, file, 0, danube, "raw_text", unitVec(1))
	e.chunk(t, e.user.ID, file, 1, capital, "raw_text", unitVec(2))
	e.chunk(t, e.user.ID, file, 2, "Рецепта за баница включва сирене, яйца и кори.", "raw_text", unitVec(3))

	rows := e.hybrid(t, "река:* & дуна:* & грани:*", unitVec(0), 5)
	require.Len(t, rows, 1, "the text half must match")
	assert.Equal(t, want, rows[0].ID)
	assert.GreaterOrEqual(t, rows[0].Score, 0.5, "an AND match of all the words scores at least 0.5")

	// All the words are required: one that is not in the chunk rules it out.
	assert.Empty(t, e.hybrid(t, "река:* & дуна:* & океа:*", unitVec(0), 5))
	// A word group alternative finds it again.
	assert.Len(t, e.hybrid(t, "(океа:* & дуна:*) | (дуна:* & румъ:*)", unitVec(0), 5), 1)
	// Nothing to look for, nothing found; and the vectors alone are far away.
	assert.Empty(t, e.hybrid(t, "", unitVec(0), 5))
	assert.Empty(t, e.hybrid(t, "квантов:* & хромодинам:*", unitVec(0), 5))
}

func TestTextOnlySearch(t *testing.T) {
	e := newSearchEnv(t)
	file := e.file(t, e.user.ID, "geo.txt", "completed", "general")
	e.chunk(t, e.user.ID, file, 0, danube, "raw_text", unitVec(1))
	want := e.chunk(t, e.user.ID, file, 1, capital, "raw_text", unitVec(2))

	search := func(tsQuery string) []db.SearchTextChunksRow {
		var rows []db.SearchTextChunksRow
		require.NoError(t, e.d.WithSearchSettings(e.ctx, func(q *db.Queries) (err error) {
			rows, err = q.SearchTextChunks(e.ctx, db.SearchTextChunksParams{UserID: e.user.ID, TsQuery: tsQuery, MatchCount: 5})
			return err
		}))
		return rows
	}

	rows := search("столиц:* & българ:*")
	require.Len(t, rows, 1)
	assert.Equal(t, want, rows[0].ID)
	assert.GreaterOrEqual(t, rows[0].Score, 0.5)
	assert.Empty(t, search(""), "an empty query matches nothing")
}

func TestHybridSearchByVector(t *testing.T) {
	e := newSearchEnv(t)
	file := e.file(t, e.user.ID, "a.txt", "completed", "general")
	near := e.chunk(t, e.user.ID, file, 0, "първи текст без общи думи", "raw_text", unitVec(5))
	e.chunk(t, e.user.ID, file, 1, "втори текст без общи думи", "raw_text", unitVec(6))

	rows := e.hybrid(t, "zzzz", unitVec(5), 5)
	require.Len(t, rows, 1, "only the chunk within the distance limit")
	assert.Equal(t, near, rows[0].ID)
	assert.InDelta(t, 1.0, rows[0].Score, 1e-6)
}

// Two chunks per file is the cap when the library has many files, but a library
// of one book must not be cut down to two chunks in total.
func TestHybridSearchPerFileCap(t *testing.T) {
	t.Run("one file returns as many as asked for", func(t *testing.T) {
		e := newSearchEnv(t)
		file := e.file(t, e.user.ID, "book.txt", "completed", "general")
		for i := 0; i < 8; i++ {
			e.chunk(t, e.user.ID, file, i, fmt.Sprintf("откъс номер %d", i), "raw_text", unitVec(10))
		}
		assert.Len(t, e.hybrid(t, "zzzz", unitVec(10), 8), 8)
	})
	t.Run("many files are capped at two each", func(t *testing.T) {
		e := newSearchEnv(t)
		for f := 0; f < 5; f++ {
			file := e.file(t, e.user.ID, fmt.Sprintf("book%d.txt", f), "completed", "general")
			for i := 0; i < 6; i++ {
				e.chunk(t, e.user.ID, file, i, fmt.Sprintf("откъс %d от книга %d", i, f), "raw_text", unitVec(10))
			}
		}
		// 5 files, 10 wanted: ceil(10/5) = 2 per file.
		rows := e.hybrid(t, "zzzz", unitVec(10), 10)
		assert.Len(t, rows, 10)
		perFile := map[string]int{}
		for _, r := range rows {
			perFile[r.FileName]++
		}
		for name, n := range perFile {
			assert.LessOrEqual(t, n, 2, "%s", name)
		}
		assert.Len(t, perFile, 5)
	})
}

func TestSearchSkipsWhatIsNotTheUsers(t *testing.T) {
	e := newSearchEnv(t)
	other, err := e.d.Queries.CreateUser(e.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.d.Pool, other.ID) })

	mine := e.file(t, e.user.ID, "mine.txt", "completed", "general")
	want := e.chunk(t, e.user.ID, mine, 0, danube, "raw_text", unitVec(1))

	theirs := e.file(t, other.ID, "theirs.txt", "completed", "general")
	e.chunk(t, other.ID, theirs, 0, danube, "raw_text", unitVec(1))
	pending := e.file(t, e.user.ID, "pending.txt", "pending", "general")
	e.chunk(t, e.user.ID, pending, 0, danube, "raw_text", unitVec(1))
	deleted := e.file(t, e.user.ID, "deleted.txt", "completed", "general")
	e.chunk(t, e.user.ID, deleted, 0, danube, "raw_text", unitVec(1))
	_, err = e.d.SqlDB.ExecContext(e.ctx, `UPDATE ingested_files SET deleted_at = now() WHERE id = $1`, deleted)
	require.NoError(t, err)

	rows := e.hybrid(t, "река:* & дуна:*", unitVec(1), 10)
	require.Len(t, rows, 1)
	assert.Equal(t, want, rows[0].ID)
}

func TestConceptSearch(t *testing.T) {
	e := newSearchEnv(t)
	file := e.file(t, e.user.ID, "book.txt", "completed", "general")
	raw := e.chunk(t, e.user.ID, file, 0, "суров текст", "raw_text", unitVec(20))
	concept := e.chunk(t, e.user.ID, file, 1, "обобщение на темата", "concept_summary", unitVec(20))
	e.chunk(t, e.user.ID, file, 2, "обобщение на друга тема", "concept_summary", unitVec(21))

	search := func(matchCount int32) []db.SearchConceptChunksRow {
		var rows []db.SearchConceptChunksRow
		require.NoError(t, e.d.WithSearchSettings(e.ctx, func(q *db.Queries) (err error) {
			rows, err = q.SearchConceptChunks(e.ctx, db.SearchConceptChunksParams{
				UserID: e.user.ID, Embedding: unitVec(20), VectorLimit: db.SearchVectorCandidates,
				MaxDistance: 0.40, MatchCount: matchCount,
			})
			return err
		}))
		return rows
	}

	rows := search(3)
	require.Len(t, rows, 1, "the raw chunk and the distant summary are not concepts within the limit")
	assert.Equal(t, concept, rows[0].ID)
	assert.NotEqual(t, raw, rows[0].ID)
	assert.InDelta(t, 0.0, rows[0].Distance, 1e-6)
}
