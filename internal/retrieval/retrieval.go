package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"
	"golang.org/x/sync/errgroup"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

const (
	// chunkMaxDistance is the cosine distance beyond which a raw chunk is not a
	// vector match for the question. Measured on 2026-11-01 with E5-small on four
	// books (5 076 chunks): the nearest relevant chunk of 20 questions was at
	// 0.100-0.174 (median 0.129), the nearest chunk of 8 questions that the books
	// do not cover at 0.168-0.236 (median 0.205). 0.20 keeps every relevant one with
	// a margin and drops the four farthest unrelated questions; the old 0.45
	// dropped nothing. Re-measure after a change of model.
	chunkMaxDistance = 0.20

	// chunkCrossLanguageMargin is added to chunkMaxDistance for a chunk written in
	// another script than the question (see the SearchHybridChunks query). With eight
	// books, three of them English, the nearest relevant chunk of a Bulgarian question
	// was at 0.105-0.171 in a Bulgarian book and at 0.168-0.212 in an English one;
	// 0.20 dropped the English answers to two of eleven questions, while a plain
	// threshold of 0.22 for every chunk gave the questions the library cannot answer
	// 5.7 results instead of 2.7. The margin widens the threshold only where the
	// language gap is.
	chunkCrossLanguageMargin = 0.03

	// conceptMaxDistance is the same for concept summaries, where the summaries are
	// only a means: their themes widen the text half of a chunk search (SearchAll,
	// SearchRawChunks), which is measured to find the right book 9 times of 11 against
	// 6 without. They are written in English, so a Bulgarian question is farther from
	// them: the nearest summary was at 0.167-0.250 (median 0.200) for the 20 questions
	// the books cover and at 0.236-0.298 (median 0.270) for the 8 they do not. The two
	// ranges overlap a little, so 0.25 cannot separate them cleanly, but it keeps every
	// summary of a covered question and drops most of the rest; the old 0.40 returned
	// three unrelated summaries for a question about the capital of France.
	conceptMaxDistance = 0.25

	// contextConceptMaxDistance is the limit for the summaries that go into the chat
	// prompt as "ideas from the books" (SearchForContext), where an unrelated summary
	// is not a harmless miss: the model is told to weave it into its answer. Measured
	// on 2026-11-06 with 56 files (16 211 summaries) on 16 questions the books answer,
	// 4 of the real chat that they answer and 21 that they do not (greetings, thanks,
	// remarks about the day, the weather, football): the nearest summary of the 21 was
	// at 0.174-0.271 (a few books - Dalio, "Chicken Soup", "The Innovators" - are the
	// nearest to almost any short Bulgarian remark), and for the questions the books
	// answer it was the right book only below 0.17 (8 of 9; from 0.18 up 1 of 7). So
	// 0.17 lets in none of the 21 and loses one useful match (0.182), but the margin to
	// the nearest unrelated one is 0.004: re-measure when files are added. The old
	// 0.25, calibrated on 4-8 books, let in 18 of the 21.
	contextConceptMaxDistance = 0.17

	// conceptMatchCount is how many concept summaries a search may return.
	conceptMatchCount = 3
)

type RetrievalService struct {
	db       *db.Database
	embedder llm.Embedder
}

func NewRetrievalService(database *db.Database, embedder llm.Embedder) *RetrievalService {
	return &RetrievalService{
		db:       database,
		embedder: embedder,
	}
}

func (s *RetrievalService) CreateMemory(ctx context.Context, userID uuid.UUID, content string, importance int, status string, metadata pqtype.NullRawMessage) (db.Memory, error) {
	if strings.TrimSpace(content) == "" {
		slog.ErrorContext(ctx, "cannot create memory with empty content")
		return db.Memory{}, fmt.Errorf("cannot create memory with empty content")
	}

	// Generate embeddings
	vectors, err := s.embedder.EmbedTexts(ctx, []string{content}, false)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate embedding", slog.Any("error", err))
		return db.Memory{}, fmt.Errorf("failed to generate embedding: %w", err)
	}

	if len(vectors) == 0 {
		slog.ErrorContext(ctx, "no embedding returned")
		return db.Memory{}, fmt.Errorf("no embedding returned")
	}
	if len(vectors[0]) != config.VectorDimension {
		slog.ErrorContext(ctx, "invalid vector dimension", slog.Int("dim", len(vectors[0])))
		return db.Memory{}, fmt.Errorf("invalid vector dimension: %d", len(vectors[0]))
	}

	pgVec := pgvector.NewVector(vectors[0])

	params := db.CreateMemoryParams{
		UserID:         userID,
		Content:        content,
		Status:         status,
		Importance:     int32(importance),
		Metadata:       metadata,
		Embedding:      pgVec,
		EmbeddingModel: config.DefaultEmbedModel,
	}

	// Create memory in database with check
	mem, err := s.db.CreateMemoryWithCheck(ctx, params)
	if err != nil {
		if errors.Is(err, db.ErrMemoryExists) {
			return mem, err
		}
		slog.ErrorContext(ctx, "failed to create memory", slog.Any("error", err))
		return db.Memory{}, fmt.Errorf("failed to create memory: %w", err)
	}

	return mem, nil
}

// We return unified results now
type SearchResult struct {
	ID         uuid.UUID
	Content    string
	SourceType string
	Score      float64
	// Distance is the cosine distance to the question, set for concept summaries.
	// Score is (1 - Distance) there, which is not a similarity worth showing.
	Distance float64
	FileName string
}

// embedQuery embeds a search query. A nil vector with a nil error means the
// embedder is unavailable and the caller should fall back to text-only search;
// an error means the embedder returned a vector that must not be used.
func (s *RetrievalService) embedQuery(ctx context.Context, query string) ([]float32, error) {
	vectors, err := s.embedder.EmbedTexts(ctx, []string{query}, true)
	if err != nil {
		slog.WarnContext(ctx, "embedder failed, falling back to text-only search", "error", err)
		return nil, nil
	}
	if len(vectors) == 0 {
		slog.ErrorContext(ctx, "embedder returned empty result")
		return nil, nil
	}
	if len(vectors[0]) != config.VectorDimension {
		slog.ErrorContext(ctx, "invalid vector dimension", slog.Int("dim", len(vectors[0])))
		return nil, fmt.Errorf("invalid vector dimension: %d", len(vectors[0]))
	}
	return vectors[0], nil
}

// searchMemories runs the hybrid memory search, or the text-only search when
// queryVec is nil.
func (s *RetrievalService) searchMemories(ctx context.Context, userID uuid.UUID, query string, limit int, queryVec []float32) ([]SearchResult, error) {
	var res []SearchResult

	if queryVec == nil {
		rows, err := s.db.Queries.SearchTextMemories(ctx, db.SearchTextMemoriesParams{
			UserID:     userID,
			QueryText:  query,
			MatchCount: int32(limit),
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed to search text memories during fallback", slog.Any("error", err))
			return nil, fmt.Errorf("failed to search text memories during fallback: %w", err)
		}
		for _, m := range rows {
			res = append(res, SearchResult{ID: m.ID, Content: m.Content, SourceType: "memory", Score: m.Score})
		}
		return res, nil
	}

	rows, err := s.db.Queries.SearchHybridMemories(ctx, db.SearchHybridMemoriesParams{
		UserID:     userID,
		MatchCount: int32(limit),
		Embedding:  pgvector.NewVector(queryVec),
		QueryText:  query,
		MinTextSim: db.SearchMinTextSimilarity,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to search hybrid memories", slog.Any("error", err))
		return nil, fmt.Errorf("failed to search hybrid memories: %w", err)
	}
	for _, row := range rows {
		res = append(res, SearchResult{ID: row.ID, Content: row.Content, SourceType: "memory", Score: row.Score})
	}
	return res, nil
}

// searchChunks runs the hybrid chunk search, or the
// text-only search when queryVec is nil. The words of query - and of themes,
// the topics of the concept summaries that matched - are what the lexical half
// looks for in the chunk text.
func (s *RetrievalService) searchChunks(ctx context.Context, userID uuid.UUID, query string, themes []string, limit int, queryVec []float32) ([]SearchResult, error) {
	var res []SearchResult
	tsQuery := lexicalQuery(query, themes)

	if queryVec == nil {
		var rows []db.SearchTextChunksRow
		err := s.db.WithSearchSettings(ctx, func(q *db.Queries) (err error) {
			rows, err = q.SearchTextChunks(ctx, db.SearchTextChunksParams{
				UserID:     userID,
				TsQuery:    tsQuery,
				MatchCount: int32(limit),
			})
			return err
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed to search text chunks during fallback", slog.Any("error", err))
			return nil, fmt.Errorf("failed to search text chunks during fallback: %w", err)
		}
		for _, m := range rows {
			res = append(res, SearchResult{ID: m.ID, Content: m.Content, SourceType: "chunk", Score: m.Score, FileName: m.FileName})
		}
		return res, nil
	}

	var rows []db.SearchHybridChunksRow
	err := s.db.WithSearchSettings(ctx, func(q *db.Queries) (err error) {
		rows, err = q.SearchHybridChunks(ctx, db.SearchHybridChunksParams{
			UserID:              userID,
			MatchCount:          int32(limit),
			Embedding:           pgvector.NewVector(queryVec),
			VectorLimit:         db.SearchVectorCandidates,
			MaxDistance:         chunkMaxDistance,
			QueryCyrillic:       queryIsCyrillic(query),
			CrossLanguageMargin: chunkCrossLanguageMargin,
			TsQuery:             tsQuery,
		})
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to search hybrid chunks", slog.Any("error", err))
		return nil, fmt.Errorf("failed to search hybrid chunks: %w", err)
	}
	for _, row := range rows {
		res = append(res, SearchResult{ID: row.ID, Content: row.Content, SourceType: "chunk", Score: row.Score, FileName: row.FileName})
	}
	return res, nil
}

// searchConcepts returns the concept-summary chunks closer to queryVec than
// maxDistance along with the themes recorded in their metadata.
func (s *RetrievalService) searchConcepts(ctx context.Context, userID uuid.UUID, queryVec []float32, maxDistance float64) ([]SearchResult, []string, error) {
	var rows []db.SearchConceptChunksRow
	err := s.db.WithSearchSettings(ctx, func(q *db.Queries) (err error) {
		rows, err = q.SearchConceptChunks(ctx, db.SearchConceptChunksParams{
			UserID:      userID,
			Embedding:   pgvector.NewVector(queryVec),
			VectorLimit: db.SearchVectorCandidates,
			MaxDistance: maxDistance,
			MatchCount:  conceptMatchCount,
		})
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	var res []SearchResult
	var themes []string
	for _, row := range rows {
		var meta struct {
			Themes []string `json:"themes"`
		}
		if err := json.Unmarshal(row.Metadata, &meta); err == nil {
			themes = append(themes, meta.Themes...)
		}
		res = append(res, SearchResult{
			ID:         row.ID,
			Content:    row.Content,
			SourceType: "concept",
			Score:      1.0 - row.Distance, // naive conversion
			Distance:   row.Distance,
			FileName:   row.FileName,
		})
	}
	return res, themes, nil
}

func (s *RetrievalService) SearchAll(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, []SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search_all").Observe(time.Since(start).Seconds())
	}()

	queryVec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, nil, err
	}

	// Associative search: concept summaries are returned alongside the chunks
	// and their themes widen the text side of the chunk search.
	var concepts []SearchResult
	var themes []string
	if queryVec != nil {
		concepts, themes, err = s.searchConcepts(ctx, userID, queryVec, conceptMaxDistance)
		if err != nil {
			slog.WarnContext(ctx, "failed to search concept chunks", "error", err)
		}
	}

	var memories, chunks []SearchResult
	eg, childCtx := errgroup.WithContext(ctx)
	eg.Go(func() (err error) {
		memories, err = s.searchMemories(childCtx, userID, query, limit, queryVec)
		return err
	})
	eg.Go(func() (err error) {
		chunks, err = s.searchChunks(childCtx, userID, query, themes, limit, queryVec)
		return err
	})
	if err := eg.Wait(); err != nil {
		return nil, nil, err
	}

	return memories, mergeConcepts(concepts, chunks), nil
}

// mergeConcepts puts concept results ahead of chunks, dropping chunks that the
// concept search already returned.
func mergeConcepts(concepts, chunks []SearchResult) []SearchResult {
	if len(concepts) == 0 {
		return chunks
	}

	seen := make(map[uuid.UUID]struct{}, len(concepts))
	merged := make([]SearchResult, 0, len(concepts)+len(chunks))
	for _, c := range concepts {
		seen[c.ID] = struct{}{}
		merged = append(merged, c)
	}
	for _, c := range chunks {
		if _, dup := seen[c.ID]; !dup {
			merged = append(merged, c)
		}
	}
	return merged
}

// CosineSimilarity is the cosine of the angle between a and b; 0 when either has
// no length. The shorter of the two decides how many dimensions are compared.
func CosineSimilarity(a, b []float32) float32 {
	var dotProduct, normA, normB float32
	for i := 0; i < len(a) && i < len(b); i++ {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

func (s *RetrievalService) SearchForContext(ctx context.Context, userID uuid.UUID, query string, limit int) (memories []SearchResult, conceptChunks []SearchResult, err error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search_for_context").Observe(time.Since(start).Seconds())
	}()

	queryVec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, nil, err
	}

	if queryVec == nil {
		memories, err = s.searchMemories(ctx, userID, query, limit, nil)
		if err != nil {
			return nil, nil, err
		}
		return memories, nil, nil
	}

	eg, childCtx := errgroup.WithContext(ctx)
	eg.Go(func() (err error) {
		memories, err = s.searchMemories(childCtx, userID, query, limit, queryVec)
		return err
	})
	eg.Go(func() error {
		var conceptErr error
		conceptChunks, _, conceptErr = s.searchConcepts(childCtx, userID, queryVec, contextConceptMaxDistance)
		if conceptErr != nil {
			slog.WarnContext(ctx, "failed to search concept chunks", "error", conceptErr)
		}
		return nil
	})
	if err := eg.Wait(); err != nil {
		return nil, nil, err
	}

	return memories, conceptChunks, nil
}

// SearchRawChunks is the search behind the search_documents tool: the chunks of
// the books that answer a question. The concept summaries closest to the question
// lend their themes to the text half of the search (as in SearchAll), which is what
// finds an English book from a Bulgarian question: on eleven questions with a known
// answer it put a chunk of the right book into the first five results 9 times,
// against 6 times without the themes. The summaries themselves are not returned.
func (s *RetrievalService) SearchRawChunks(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search_raw_chunks").Observe(time.Since(start).Seconds())
	}()

	queryVec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}

	var themes []string
	if queryVec != nil {
		// The themes only widen the search; without them it still works.
		if _, themes, err = s.searchConcepts(ctx, userID, queryVec, conceptMaxDistance); err != nil {
			slog.WarnContext(ctx, "failed to search concept chunks for themes", "error", err)
			themes = nil
		}
	}
	return s.searchChunks(ctx, userID, query, themes, limit, queryVec)
}

// queryIsCyrillic reports whether text is written mostly in Cyrillic, which is
// what the language-aware distance threshold compares the chunks against. Text
// without letters counts as Cyrillic (the language of the owner's questions).
func queryIsCyrillic(text string) bool {
	cyrillic, latin := 0, 0
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}
	return cyrillic >= latin
}

func (s *RetrievalService) SearchMemories(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search_memories").Observe(time.Since(start).Seconds())
	}()

	queryVec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.searchMemories(ctx, userID, query, limit, queryVec)
}

func (s *RetrievalService) SearchChunks(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
	_, chunks, err := s.SearchAll(ctx, userID, query, limit)
	return chunks, err
}
