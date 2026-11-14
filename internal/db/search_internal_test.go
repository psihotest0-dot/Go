package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/dbtest"
)

// hnsw.ef_search must be in force inside the search transaction and gone outside
// it, so it cannot leak into other users of the pooled connection.
func TestSearchSettingsAreScopedToTheTransaction(t *testing.T) {
	ctx := context.Background()
	d, err := Connect(ctx, dbtest.ConnString(t))
	require.NoError(t, err)
	defer d.Close()

	show := func(tx *sql.Tx) string {
		var v string
		require.NoError(t, tx.QueryRowContext(ctx, `SHOW hnsw.ef_search`).Scan(&v))
		return v
	}

	for i := 0; i < 3; i++ { // the pool may hand back the same connection each time
		var inside string
		require.NoError(t, d.inSearchTx(ctx, func(tx *sql.Tx) error {
			inside = show(tx)
			return nil
		}))
		assert.Equal(t, fmt.Sprint(SearchEfSearch), inside)

		tx, err := d.SqlDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		// pgvector's default is 40, or an empty placeholder while its library is not
		// yet loaded in this session; what matters is that it is not our value.
		assert.NotEqual(t, fmt.Sprint(SearchEfSearch), show(tx), "the setting must not outlive the transaction")
		require.NoError(t, tx.Rollback())
	}
}

func TestSearchTransactionIsReadOnly(t *testing.T) {
	ctx := context.Background()
	d, err := Connect(ctx, dbtest.ConnString(t))
	require.NoError(t, err)
	defer d.Close()

	err = d.inSearchTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE should_not_exist (x int)`)
		return err
	})
	require.Error(t, err, "a search must not be able to write")
}

// vec returns a vector with the given weights at the given positions.
func vec(weights map[int]float32) pgvector.Vector {
	v := make([]float32, config.VectorDimension)
	for i, w := range weights {
		v[i] = w
	}
	return pgvector.NewVector(v)
}

// The two chunk types share one table but not one index. With a single index
// over both, the nearest 200 vectors to a question were all raw chunks and the
// concept summaries (written in another language, so farther) were never among
// them: the concept search returned nothing, in production, for every question.
func TestSearchesSeeTheirOwnChunkTypeBehindManyOfTheOther(t *testing.T) {
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, RunMigrations(connString))
	ctx := context.Background()
	d, err := Connect(ctx, connString)
	require.NoError(t, err)
	defer d.Close()
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	defer dbtest.DeleteUser(t, d.Pool, user.ID)

	var fileID uuid.UUID
	require.NoError(t, d.SqlDB.QueryRowContext(ctx,
		`INSERT INTO ingested_files (user_id, file_name, file_hash, mime_type, file_size, status, local_path)
		 VALUES ($1, 'book.txt', $2, 'text/plain', 1, 'completed', 'x') RETURNING id`, user.ID, "h-"+uuid.NewString()).Scan(&fileID))

	insert := func(chunkType string, n int, v pgvector.Vector, firstIndex int) {
		_, err := d.SqlDB.ExecContext(ctx,
			`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
			 SELECT $1, $2, $3 + g, $4 || g, $5, $6, '{}' FROM generate_series(1, $7) g`,
			fileID, user.ID, firstIndex, chunkType+" ", v, chunkType, n)
		require.NoError(t, err)
	}
	// pgvector 0.5 keeps the index entries of deleted rows until VACUUM, and they
	// count against the candidates an index scan returns: after the deletes of an
	// earlier run (or of the other half of this test) a search could see only dead
	// rows. Autovacuum does this in production; the test does not wait for it.
	vacuum := func() {
		_, err := d.SqlDB.ExecContext(ctx, `VACUUM document_chunks`)
		require.NoError(t, err)
	}
	clear := func() {
		_, err := d.SqlDB.ExecContext(ctx, `DELETE FROM document_chunks WHERE file_id = $1`, fileID)
		require.NoError(t, err)
		vacuum()
	}
	vacuum()
	query := vec(map[int]float32{40: 1})
	near := query                             // distance 0 from the question
	far := vec(map[int]float32{40: 2, 41: 1}) // distance 0.106, behind every near vector

	type counts struct{ concepts, raw int }
	search := func() counts {
		var got counts
		// On a table this small the planner would rather scan and sort. A large
		// library forces the ordered index scan; these settings force it here.
		require.NoError(t, d.inSearchTx(ctx, func(tx *sql.Tx) error {
			for _, setting := range []string{"enable_seqscan", "enable_bitmapscan", "enable_sort"} {
				_, err := tx.ExecContext(ctx, "SET LOCAL "+setting+" = off")
				require.NoError(t, err)
			}
			q := d.Queries.WithTx(tx)
			concepts, err := q.SearchConceptChunks(ctx, SearchConceptChunksParams{
				UserID: user.ID, Embedding: query, VectorLimit: SearchVectorCandidates, MaxDistance: 0.4, MatchCount: 1000,
			})
			require.NoError(t, err)
			raw, err := q.SearchHybridChunks(ctx, SearchHybridChunksParams{
				UserID: user.ID, Embedding: query, VectorLimit: SearchVectorCandidates, MaxDistance: 0.4, MatchCount: 1000,
			})
			require.NoError(t, err)
			got = counts{concepts: len(concepts), raw: len(raw)}
			return nil
		}))
		return got
	}

	// More raw chunks than the candidates asked for, all nearer to the question than 5 summaries: the summaries still show.
	insert("raw_text", SearchVectorCandidates+100, near, 0)
	insert("concept_summary", 5, far, 1000)
	got := search()
	assert.Equal(t, 5, got.concepts, "the summaries were crowded out by the nearer raw chunks")
	assert.Equal(t, 100, got.raw, "the hybrid search takes 100 raw chunks per side")
	clear()

	// And the other way round: more summaries than the candidates, nearer than 5 raw chunks.
	insert("concept_summary", SearchVectorCandidates+100, near, 1000)
	insert("raw_text", 5, far, 0)
	got = search()
	assert.Equal(t, 5, got.raw, "the raw chunks were crowded out by the nearer summaries")
	assert.Equal(t, SearchVectorCandidates, got.concepts, "a search returns at most the candidates asked for")

	// The plan is the partial index of each type.
	require.NoError(t, d.inSearchTx(ctx, func(tx *sql.Tx) error {
		for _, setting := range []string{"enable_seqscan", "enable_bitmapscan", "enable_sort"} {
			_, err := tx.ExecContext(ctx, "SET LOCAL "+setting+" = off")
			require.NoError(t, err)
		}
		plan := func(sqlText string, args ...any) string {
			rows, err := tx.QueryContext(ctx, "EXPLAIN "+sqlText, args...)
			require.NoError(t, err)
			defer rows.Close()
			var sb strings.Builder
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				sb.WriteString(line + "\n")
			}
			return sb.String()
		}
		conceptPlan := plan(searchConceptChunks, user.ID, 0.4, 10, query, 200)
		assert.Contains(t, conceptPlan, "idx_document_chunks_concept_embedding", conceptPlan)
		hybridPlan := plan(searchHybridChunks, user.ID, 10, query, 200, 0.4, true, 0.03, "")
		assert.Contains(t, hybridPlan, "idx_document_chunks_raw_cyrillic_embedding", hybridPlan)
		assert.Contains(t, hybridPlan, "idx_document_chunks_raw_other_embedding", hybridPlan)
		return nil
	}))
}

// The vector candidates come from the Cyrillic chunks and the others separately.
// From one pool, a Bulgarian question near hundreds of Bulgarian passages had no
// room left for an English book that was a little farther (the usual distance
// across languages), and in production one HNSW graph over both scripts did not
// even reach the English passages that were the nearest of all.
func TestHybridSearchTakesCandidatesFromEachScript(t *testing.T) {
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	require.NoError(t, RunMigrations(connString))
	ctx := context.Background()
	d, err := Connect(ctx, connString)
	require.NoError(t, err)
	defer d.Close()
	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)
	defer dbtest.DeleteUser(t, d.Pool, user.ID)

	newFile := func(name string) uuid.UUID {
		var id uuid.UUID
		require.NoError(t, d.SqlDB.QueryRowContext(ctx,
			`INSERT INTO ingested_files (user_id, file_name, file_hash, mime_type, file_size, status, local_path)
			 VALUES ($1, $2, $3, 'text/plain', 1, 'completed', 'x') RETURNING id`, user.ID, name, "h-"+uuid.NewString()).Scan(&id))
		return id
	}
	bulgarian, english := newFile("bulgarian.txt"), newFile("english.txt")
	insert := func(fileID uuid.UUID, text string, n int, v pgvector.Vector) {
		_, err := d.SqlDB.ExecContext(ctx,
			`INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
			 SELECT $1, $2, g, $3 || g, $4, 'raw_text', '{}' FROM generate_series(1, $5) g`,
			fileID, user.ID, text, v, n)
		require.NoError(t, err)
	}
	_, err = d.SqlDB.ExecContext(ctx, `VACUUM document_chunks`)
	require.NoError(t, err)

	query := vec(map[int]float32{40: 1})
	insert(bulgarian, "Питър Пан живее в Небивала земя ", SearchVectorCandidates+100, query) // distance 0
	insert(english, "Peter Pan lives in Neverland ", 5, vec(map[int]float32{40: 2, 41: 1}))  // distance 0.106

	var files []string
	require.NoError(t, d.inSearchTx(ctx, func(tx *sql.Tx) error {
		for _, setting := range []string{"enable_seqscan", "enable_bitmapscan", "enable_sort"} {
			_, err := tx.ExecContext(ctx, "SET LOCAL "+setting+" = off")
			require.NoError(t, err)
		}
		rows, err := d.Queries.WithTx(tx).SearchHybridChunks(ctx, SearchHybridChunksParams{
			UserID: user.ID, Embedding: query, VectorLimit: SearchVectorCandidates, MaxDistance: 0.2,
			QueryCyrillic: true, CrossLanguageMargin: 0.03, MatchCount: 8,
		})
		require.NoError(t, err)
		for _, r := range rows {
			files = append(files, r.FileName)
		}
		return nil
	}))
	assert.Contains(t, files, "english.txt", "the English book was crowded out of the candidates by the nearer Bulgarian chunks")
	assert.Contains(t, files, "bulgarian.txt")
}
