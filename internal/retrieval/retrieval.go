package retrieval

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"

	"time"

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

func (s *RetrievalService) SearchSimilar(ctx context.Context, userID uuid.UUID, query string, limit int) ([]db.Memory, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search").Observe(time.Since(start).Seconds())
	}()

	vectors, err := s.embedder.EmbedTexts(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("failed to generate embedding for query: %w", err)
	}

	if len(vectors) == 0 {
		return nil, fmt.Errorf("no embedding returned for query")
	}

	pgVec := pgvector.NewVector(vectors[0])

	params := db.SearchHybridMemoriesParams{
		UserID:     userID,
		MatchCount: int32(limit),
		Embedding:  pgVec,
		QueryText:  query,
	}

	memories, err := s.db.Queries.SearchHybridMemories(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to search hybrid memories: %w", err)
	}

	return memories, nil
}
