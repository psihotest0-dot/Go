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
	// categoryBoostMargin is how far the best category centroid must be ahead
	// of the runner-up before its chunks get boosted.
	categoryBoostMargin = 0.08
	// categoryBoostAmount is the score bonus given to chunks of the boosted category.
	categoryBoostAmount = 0.08
)

type RetrievalService struct {
	db       *db.Database
	embedder llm.Embedder
	catMgr   *CategoryManager
}

func NewRetrievalService(database *db.Database, embedder llm.Embedder, catMgr *CategoryManager) *RetrievalService {
	return &RetrievalService{
		db:       database,
		embedder: embedder,
		catMgr:   catMgr,
	}
}

func (s *RetrievalService) CreateMemory(ctx context.Context, userID uuid.UUID, content string, importance int, status string, metadata pqtype.NullRawMessage) (db.Memory, error) {
	if strings.TrimSpace(content) == "" {
		slog.Error("cannot create memory with empty content")
		return db.Memory{}, fmt.Errorf("cannot create memory with empty content")
	}

	// Generate embeddings
	vectors, err := s.embedder.EmbedTexts(ctx, []string{content}, false)
	if err != nil {
		slog.Error("failed to generate embedding", slog.Any("error", err))
		return db.Memory{}, fmt.Errorf("failed to generate embedding: %w", err)
	}

	if len(vectors) == 0 {
		slog.Error("no embedding returned")
		return db.Memory{}, fmt.Errorf("no embedding returned")
	}
	if len(vectors[0]) != config.VectorDimension {
		slog.Error("invalid vector dimension", slog.Int("dim", len(vectors[0])))
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
		slog.Error("failed to create memory", slog.Any("error", err))
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
	FileName   string
}

// embedQuery embeds a search query. A nil vector with a nil error means the
// embedder is unavailable and the caller should fall back to text-only search;
// an error means the embedder returned a vector that must not be used.
func (s *RetrievalService) embedQuery(ctx context.Context, query string) ([]float32, error) {
	vectors, err := s.embedder.EmbedTexts(ctx, []string{query}, true)
	if err != nil {
		slog.Warn("embedder failed, falling back to text-only search", "error", err)
		return nil, nil
	}
	if len(vectors) == 0 {
		slog.Error("embedder returned empty result")
		return nil, nil
	}
	if len(vectors[0]) != config.VectorDimension {
		slog.Error("invalid vector dimension", slog.Int("dim", len(vectors[0])))
		return nil, fmt.Errorf("invalid vector dimension: %d", len(vectors[0]))
	}
	return vectors[0], nil
}

// boostFor returns the category whose chunks should be boosted for queryVec,
// or an empty category when no centroid clearly wins.
func (s *RetrievalService) boostFor(queryVec []float32) (string, float64) {
	if s.catMgr == nil {
		return "", 0
	}

	bestCat := ""
	bestScore := float32(-1.0)
	secondBestScore := float32(-1.0)

	for cat, centroid := range s.catMgr.GetAllCentroids() {
		score := cosineSimilarity(queryVec, centroid)
		if score > bestScore {
			secondBestScore = bestScore
			bestScore = score
			bestCat = cat
		} else if score > secondBestScore {
			secondBestScore = score
		}
	}

	if bestScore-secondBestScore >= categoryBoostMargin {
		return bestCat, categoryBoostAmount
	}
	return "", 0
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
			slog.Error("failed to search text memories during fallback", slog.Any("error", err))
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
	})
	if err != nil {
		slog.Error("failed to search hybrid memories", slog.Any("error", err))
		return nil, fmt.Errorf("failed to search hybrid memories: %w", err)
	}
	for _, row := range rows {
		res = append(res, SearchResult{ID: row.ID, Content: row.Content, SourceType: "memory", Score: row.Score})
	}
	return res, nil
}

// searchChunks runs the hybrid chunk search with category boosting, or the
// text-only search when queryVec is nil. query is the text matched against
// chunk text; it may be wider than the text that was embedded.
func (s *RetrievalService) searchChunks(ctx context.Context, userID uuid.UUID, query string, limit int, queryVec []float32) ([]SearchResult, error) {
	var res []SearchResult

	if queryVec == nil {
		rows, err := s.db.Queries.SearchTextChunks(ctx, db.SearchTextChunksParams{
			UserID:     userID,
			QueryText:  query,
			MatchCount: int32(limit),
		})
		if err != nil {
			slog.Error("failed to search text chunks during fallback", slog.Any("error", err))
			return nil, fmt.Errorf("failed to search text chunks during fallback: %w", err)
		}
		for _, m := range rows {
			res = append(res, SearchResult{ID: m.ID, Content: m.Content, SourceType: "chunk", Score: m.Score, FileName: m.FileName})
		}
		return res, nil
	}

	boostCategory, boostAmount := s.boostFor(queryVec)
	rows, err := s.db.Queries.SearchHybridChunks(ctx, db.SearchHybridChunksParams{
		UserID:        userID,
		MatchCount:    int32(limit),
		Embedding:     pgvector.NewVector(queryVec),
		QueryText:     query,
		BoostCategory: boostCategory,
		BoostAmount:   boostAmount,
	})
	if err != nil {
		slog.Error("failed to search hybrid chunks", slog.Any("error", err))
		return nil, fmt.Errorf("failed to search hybrid chunks: %w", err)
	}
	for _, row := range rows {
		res = append(res, SearchResult{ID: row.ID, Content: row.Content, SourceType: "chunk", Score: row.Score, FileName: row.FileName})
	}
	return res, nil
}

// searchConcepts returns the concept-summary chunks closest to queryVec along
// with the themes recorded in their metadata.
func (s *RetrievalService) searchConcepts(ctx context.Context, userID uuid.UUID, queryVec []float32) ([]SearchResult, []string, error) {
	rows, err := s.db.Queries.SearchConceptChunks(ctx, db.SearchConceptChunksParams{
		UserID:    userID,
		Embedding: pgvector.NewVector(queryVec),
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
	chunkQuery := query
	if queryVec != nil {
		var themes []string
		concepts, themes, err = s.searchConcepts(ctx, userID, queryVec)
		if err != nil {
			slog.Warn("failed to search concept chunks", "error", err)
		}
		if len(themes) > 0 {
			chunkQuery += " " + strings.Join(themes, " ")
		}
	}

	var memories, chunks []SearchResult
	eg, childCtx := errgroup.WithContext(ctx)
	eg.Go(func() (err error) {
		memories, err = s.searchMemories(childCtx, userID, query, limit, queryVec)
		return err
	})
	eg.Go(func() (err error) {
		chunks, err = s.searchChunks(childCtx, userID, chunkQuery, limit, queryVec)
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

func cosineSimilarity(a, b []float32) float32 {
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
		conceptChunks, _, conceptErr = s.searchConcepts(childCtx, userID, queryVec)
		if conceptErr != nil {
			slog.Warn("failed to search concept chunks", "error", conceptErr)
		}
		return nil
	})
	if err := eg.Wait(); err != nil {
		return nil, nil, err
	}

	return memories, conceptChunks, nil
}

func (s *RetrievalService) SearchRawChunks(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search_raw_chunks").Observe(time.Since(start).Seconds())
	}()

	queryVec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.searchChunks(ctx, userID, query, limit, queryVec)
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
