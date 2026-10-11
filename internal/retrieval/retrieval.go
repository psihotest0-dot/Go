package retrieval

import (
	"context"
	"fmt"
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
}

func NewRetrievalService(database *db.Database, embedder llm.Embedder) *RetrievalService {
	return &RetrievalService{
		db:       database,
		embedder: embedder,
	}
}

func (s *RetrievalService) CreateMemory(ctx context.Context, userID uuid.UUID, content string, importance int, status string, metadata pqtype.NullRawMessage) (db.Memory, error) {
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

type SearchResult struct {
	ID    uuid.UUID `json:"id"`
	Type  string    `json:"type"` // "memory" or "chunk"
	Text  string    `json:"text"`
	Score float64   `json:"score"`
}

func (s *RetrievalService) SearchSimilar(ctx context.Context, userID uuid.UUID, query string, limit int) ([]SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search").Observe(time.Since(start).Seconds())
	}()

	vectors, err := s.embedder.EmbedTexts(ctx, []string{query})
	if err != nil || len(vectors) == 0 {
		// Fallback to text search if embedder times out or fails
		fmt.Printf("Fallback to text search due to embedder error: %v\n", err)
		params := db.SearchTextMemoriesParams{
			UserID:     userID,
			QueryText:  query,
			MatchCount: int32(limit),
		}
		memories, searchErr := s.db.Queries.SearchTextMemories(ctx, params)
		if searchErr != nil {
			return nil, fmt.Errorf("failed to search text memories during fallback: %w", searchErr)
		}

		var results []SearchResult
		for _, m := range memories {
			results = append(results, SearchResult{
				ID: m.ID,
				Type: "memory",
				Text: m.Content,
				Score: 1.0, // Text search doesn't return score
			})
		}
		return results, nil
	}

	pgVec := pgvector.NewVector(vectors[0])

	params := db.SearchHybridMemoriesParams{
		UserID:     userID,
		MatchCount: int32(limit),
		Embedding:  pgVec,
		QueryText:  query,
	}

	rawResults, err := s.db.Queries.SearchHybridMemories(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to search hybrid memories: %w", err)
	}

	var results []SearchResult
	for _, r := range rawResults {
		typ := "memory"
		var id uuid.UUID

		if r.ChunkID.Valid {
			typ = "chunk"
			id = r.ChunkID.UUID
		} else if r.MemoryID.Valid {
			id = r.MemoryID.UUID
		} else {
            id = uuid.Nil
        }

		results = append(results, SearchResult{
			ID: id,
			Type: typ,
			Text: r.Text,
			Score: float64(r.Score),
		})
	}

	return results, nil
}
