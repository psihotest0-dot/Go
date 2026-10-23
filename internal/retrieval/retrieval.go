package retrieval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"
	"golang.org/x/sync/errgroup"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/metrics"
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
	vectors, err := s.embedder.EmbedTexts(ctx, []string{content})
	if err != nil {
		slog.Error("failed to generate embedding", slog.Any("error", err))
		return db.Memory{}, fmt.Errorf("failed to generate embedding: %w", err)
	}

	if len(vectors) == 0 {
		slog.Error("no embedding returned")
		return db.Memory{}, fmt.Errorf("no embedding returned")
	}
	if len(vectors[0]) != 384 {
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
		EmbeddingModel: "paraphrase-multilingual-MiniLM-L12-v2", // Example model name based on context
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

func (s *RetrievalService) SearchAll(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, []SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search_all").Observe(time.Since(start).Seconds())
	}()

	vectors, err := s.embedder.EmbedTexts(ctx, []string{query})

	fallback := false
	if err != nil {
		slog.Warn("embedder failed, falling back to text-only search", "error", err)
		fallback = true
	} else if len(vectors) == 0 {
		slog.Error("embedder returned empty result")
		fallback = true
	} else if len(vectors[0]) != 384 {
		slog.Error("invalid vector dimension", slog.Int("dim", len(vectors[0])))
		return nil, nil, fmt.Errorf("invalid vector dimension: %d", len(vectors[0]))
	}

	var memories []SearchResult
	var chunks []SearchResult

	eg, childCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex

	if fallback {
		eg.Go(func() error {
			params := db.SearchTextMemoriesParams{
				UserID:     userID,
				QueryText:  query,
				MatchCount: int32(limit),
			}
			rows, searchErr := s.db.Queries.SearchTextMemories(childCtx, params)
			if searchErr != nil {
				slog.Error("failed to search text memories during fallback", slog.Any("error", searchErr))
				return fmt.Errorf("failed to search text memories during fallback: %w", searchErr)
			}
			var res []SearchResult
			for _, m := range rows {
				res = append(res, SearchResult{
					ID:         m.ID,
					Content:    m.Content,
					SourceType: "memory",
					Score:      m.Score,
				})
			}
			mu.Lock()
			memories = res
			mu.Unlock()
			return nil
		})

		eg.Go(func() error {
			params := db.SearchTextChunksParams{
				UserID:     userID,
				QueryText:  query,
				MatchCount: int32(limit),
			}
			rows, searchErr := s.db.Queries.SearchTextChunks(childCtx, params)
			if searchErr != nil {
				slog.Error("failed to search text chunks during fallback", slog.Any("error", searchErr))
				return fmt.Errorf("failed to search text chunks during fallback: %w", searchErr)
			}
			var res []SearchResult
			for _, m := range rows {
				res = append(res, SearchResult{
					ID:         m.ID,
					Content:    m.Content,
					SourceType: "chunk",
					Score:      m.Score,
					FileName:   m.FileName,
				})
			}
			mu.Lock()
			chunks = res
			mu.Unlock()
			return nil
		})

		if err := eg.Wait(); err != nil {
			return nil, nil, err
		}
		return memories, chunks, nil
	}

	queryVec := vectors[0]
	pgVec := pgvector.NewVector(queryVec)

	boostCategory := ""
	boostAmount := 0.0

	if s.catMgr != nil {
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

		margin := bestScore - secondBestScore
		if margin >= 0.08 {
			boostCategory = bestCat
			boostAmount = 0.08
		}
	}

	eg.Go(func() error {
		params := db.SearchHybridMemoriesParams{
			UserID:     userID,
			MatchCount: int32(limit),
			Embedding:  pgVec,
			QueryText:  query,
		}
		rows, err := s.db.Queries.SearchHybridMemories(childCtx, params)
		if err != nil {
			slog.Error("failed to search hybrid memories", slog.Any("error", err))
			return fmt.Errorf("failed to search hybrid memories: %w", err)
		}
		var res []SearchResult
		for _, row := range rows {
			res = append(res, SearchResult{
				ID:         row.ID,
				Content:    row.Content,
				SourceType: "memory",
				Score:      row.Score,
			})
		}
		mu.Lock()
		memories = res
		mu.Unlock()
		return nil
	})

	eg.Go(func() error {
		params := db.SearchHybridChunksParams{
			UserID:        userID,
			MatchCount:    int32(limit),
			Embedding:     pgVec,
			QueryText:     query,
			BoostCategory: boostCategory,
			BoostAmount:   boostAmount,
		}
		rows, err := s.db.Queries.SearchHybridChunks(childCtx, params)
		if err != nil {
			slog.Error("failed to search hybrid chunks", slog.Any("error", err))
			return fmt.Errorf("failed to search hybrid chunks: %w", err)
		}
		var res []SearchResult
		for _, row := range rows {
			res = append(res, SearchResult{
				ID:         row.ID,
				Content:    row.Content,
				SourceType: "chunk",
				Score:      row.Score,
				FileName:   row.FileName,
			})
		}
		mu.Lock()
		chunks = res
		mu.Unlock()
		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, nil, err
	}

	return memories, chunks, nil
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

func (s *RetrievalService) SearchMemories(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
    memories, _, err := s.SearchAll(ctx, userID, query, limit)
    return memories, err
}

func (s *RetrievalService) SearchChunks(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
    _, chunks, err := s.SearchAll(ctx, userID, query, limit)
    return chunks, err
}
