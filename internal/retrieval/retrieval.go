package retrieval

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"

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
		return db.Memory{}, fmt.Errorf("cannot create memory with empty content")
	}

	// Generate embeddings
	vectors, err := s.embedder.EmbedTexts(ctx, []string{content})
	if err != nil {
		return db.Memory{}, fmt.Errorf("failed to generate embedding: %w", err)
	}

	if len(vectors) == 0 {
		return db.Memory{}, fmt.Errorf("no embedding returned")
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
}

func (s *RetrievalService) SearchSimilar(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search").Observe(time.Since(start).Seconds())
	}()

	var results []SearchResult

	vectors, err := s.embedder.EmbedTexts(ctx, []string{query})
	if err != nil || len(vectors) == 0 {
		// Fallback to text search if embedder times out or fails
		fmt.Printf("Fallback to text search due to embedder error: %v\n", err)
		params := db.SearchTextAllParams{
			UserID:     userID,
			QueryText:  query,
			MatchCount: int32(limit),
		}
		rows, searchErr := s.db.Queries.SearchTextAll(ctx, params)
		if searchErr != nil {
			return nil, fmt.Errorf("failed to search text memories during fallback: %w", searchErr)
		}

		for _, m := range rows {
			results = append(results, SearchResult{
				ID:         m.ID,
				Content:    m.Content,
				SourceType: m.SourceType,
				Score:      0, // text fallback doesn't map directly to RRF score
			})
		}
		return results, nil
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

	params := db.SearchHybridAllParams{
		UserID:        userID,
		MatchCount:    int32(limit),
		Embedding:     pgVec,
		QueryText:     query,
		BoostCategory: boostCategory,
		BoostAmount:   boostAmount,
	}

	rows, err := s.db.Queries.SearchHybridAll(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to search hybrid memories: %w", err)
	}

	for _, row := range rows {
		results = append(results, SearchResult{
			ID:         row.ID,
			Content:    row.Content,
			SourceType: row.SourceType,
			Score:      row.Score,
		})
	}

	return results, nil
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
