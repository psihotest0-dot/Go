package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/lib/pq"
	pgvector "github.com/pgvector/pgvector-go"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/metrics"
)

// Embedder defines the interface for embedding texts
type Embedder interface {
	EmbedTexts(ctx context.Context, texts []string, isQuery bool) ([][]float32, error)
}

type HTTPEmbedder struct {
	baseURL string
	db      *db.Database
	client  *http.Client
}

func NewHTTPEmbedder(database *db.Database) *HTTPEmbedder {
	baseURL := os.Getenv("EMBEDDER_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8000"
	}

	client := &http.Client{}

	// Wait for embedder to be ready with retries
	ready := false
	for i := 0; i < 3; i++ {
		resp, err := client.Get(baseURL + "/health")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			ready = true
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(1 * time.Second)
	}

	if !ready {
		// Just log or ignore, the actual EmbedTexts will fail later if it's still down.
	}

	return &HTTPEmbedder{
		baseURL: baseURL,
		db:      database,
		client:  client,
	}
}

type EmbedRequest struct {
	Texts []string `json:"texts"`
}

type EmbedResponse struct {
	Model   string      `json:"model"`
	Dim     int         `json:"dim"`
	Vectors [][]float32 `json:"vectors"`
}

// ErrEmbedderModel is returned when the embedding service answers with a model
// other than the one the vectors in the database come from. Vectors of different
// models are not comparable, so a mismatch must stop the work instead of storing
// them: on 2026-11-01 the container turned out to run a different model than the
// code, the database and the documentation said, and nothing noticed.
var ErrEmbedderModel = errors.New("the embedding service runs a different model than expected")

// cacheKey is the key of a text in embedding_cache. The model is part of it: the
// text alone (the old key) returned the vector of whatever model had embedded it
// first, so after a change of model the cache would have mixed both.
func cacheKey(model, prefixedText string) string {
	return hashContent(model + "\n" + prefixedText)
}

func hashContent(content string) string {
	h := sha256.New()
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// CheckModel asks the service which model it runs and reports a mismatch with
// config.DefaultEmbedModel (ErrEmbedderModel). An unreachable service is an
// ordinary error; callers log both and carry on, since EmbedTexts refuses a
// wrong model by itself.
func (c *HTTPEmbedder) CheckModel(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("embedder health check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embedder health check returned status %d", resp.StatusCode)
	}
	var health EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("failed to decode the embedder health response: %w", err)
	}
	return checkEmbedResponse(health)
}

// checkEmbedResponse rejects an answer from the wrong model or of the wrong size.
func checkEmbedResponse(r EmbedResponse) error {
	if r.Model != config.DefaultEmbedModel {
		return fmt.Errorf("%w: it runs %q, the database holds vectors of %q (rebuild the embedder image from services/embedder)",
			ErrEmbedderModel, r.Model, config.DefaultEmbedModel)
	}
	if r.Dim != 0 && r.Dim != config.VectorDimension {
		return fmt.Errorf("%w: it returns %d-dimensional vectors, the database expects %d",
			ErrEmbedderModel, r.Dim, config.VectorDimension)
	}
	return nil
}

func (c *HTTPEmbedder) EmbedTexts(ctx context.Context, texts []string, isQuery bool) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("embed").Observe(time.Since(start).Seconds())
	}()

	if len(texts) == 0 {
		return nil, nil
	}

	prefixedTexts := make([]string, len(texts))
	prefix := "passage: "
	if isQuery {
		prefix = "query: "
	}
	for i, text := range texts {
		prefixedTexts[i] = prefix + text
	}

	resultVectors := make([][]float32, len(texts))
	textsToEmbed := []string{}
	indicesToEmbed := []int{}

	// 1. Check cache for each prefixed text
	for i, text := range prefixedTexts {
		h := cacheKey(config.DefaultEmbedModel, text)
		cached, err := c.db.Queries.GetEmbeddingCacheByHash(ctx, h)
		if err == nil {
			// Found in cache
			metrics.CacheHits.Inc()
			resultVectors[i] = cached.Embedding.Slice()
			continue
		}

		metrics.CacheMisses.Inc()
		textsToEmbed = append(textsToEmbed, text)
		indicesToEmbed = append(indicesToEmbed, i)
	}

	// 2. Request embeddings for non-cached texts
	if len(textsToEmbed) > 0 {
		reqBody := EmbedRequest{Texts: textsToEmbed}
		jsonBody, _ := json.Marshal(reqBody)

		req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/embed", bytes.NewBuffer(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("embed request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("embed service returned status %d", resp.StatusCode)
		}

		var embedResp EmbedResponse
		if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		if err := checkEmbedResponse(embedResp); err != nil {
			// Neither the vectors nor the cache may take anything from this answer.
			return nil, err
		}

		if len(embedResp.Vectors) != len(textsToEmbed) {
			return nil, fmt.Errorf("returned vectors count %d does not match request %d", len(embedResp.Vectors), len(textsToEmbed))
		}

		// Update cache and result
		for i, vec := range embedResp.Vectors {
			origIdx := indicesToEmbed[i]
			resultVectors[origIdx] = vec

			h := cacheKey(config.DefaultEmbedModel, textsToEmbed[i])
			pgVec := pgvector.NewVector(vec)

			_, cacheErr := c.db.Queries.CreateEmbeddingCache(ctx, db.CreateEmbeddingCacheParams{
				ContentHash: h,
				Embedding:   pgVec,
			})
			if cacheErr != nil {
				var pqErr *pq.Error
				if errors.As(cacheErr, &pqErr) && pqErr.Code == "23505" {
					// Ignore unique constraint violation from concurrent writes
				} else {
					slog.Warn("Failed to cache embedding", "error", cacheErr, "hash", h)
				}
			}
		}
	}

	return resultVectors, nil
}
