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
	// timeout bounds one EmbedTexts call as a whole: the cache read, the request to
	// the embedding service and the cache write.
	timeout time.Duration
}

// NewHTTPEmbedder creates the client of the embedding service named by
// EMBEDDER_URL. It does not wait for the service: a service that is not up yet
// is reported by CheckModel (the callers log it) and by the first EmbedTexts.
func NewHTTPEmbedder(database *db.Database) *HTTPEmbedder {
	baseURL := os.Getenv("EMBEDDER_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8000"
	}

	return &HTTPEmbedder{
		baseURL: baseURL,
		db:      database,
		client:  &http.Client{},
		timeout: config.DefaultEmbedderTimeout,
	}
}

// WithTimeout sets the limit of one EmbedTexts call (config.EmbedderTimeout). A
// slow CPU, or the OCR and the model running at the same time, can need more
// than the default for a batch of long texts.
func (c *HTTPEmbedder) WithTimeout(d time.Duration) *HTTPEmbedder {
	if d > 0 {
		c.timeout = d
	}
	return c
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
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("embed").Observe(time.Since(start).Seconds())
	}()

	if len(texts) == 0 {
		return nil, nil
	}

	prefix := "passage: "
	if isQuery {
		prefix = "query: "
	}
	prefixed := make([]string, len(texts))
	keys := make([]string, len(texts))
	for i, text := range texts {
		prefixed[i] = prefix + text
		keys[i] = cacheKey(config.DefaultEmbedModel, prefixed[i])
	}

	// 1. One cache read for the whole batch.
	cached := c.readCache(ctx, keys)

	resultVectors := make([][]float32, len(texts))
	// The same text twice in a batch is embedded once.
	var missKeys, missTexts []string
	missIndices := map[string][]int{}
	var hitKeys []string
	for i, key := range keys {
		if vec, ok := cached[key]; ok {
			metrics.CacheHits.Inc()
			resultVectors[i] = vec
			hitKeys = append(hitKeys, key)
			continue
		}

		metrics.CacheMisses.Inc()
		if _, seen := missIndices[key]; !seen {
			missKeys = append(missKeys, key)
			missTexts = append(missTexts, prefixed[i])
		}
		missIndices[key] = append(missIndices[key], i)
	}

	// Entries that were used again stay in the cache (see the cleaning by last use).
	if len(hitKeys) > 0 {
		if err := c.db.Queries.TouchEmbeddingCache(ctx, hitKeys); err != nil {
			slog.Warn("Failed to renew embedding cache entries", "error", err)
		}
	}

	// 2. Request embeddings for the texts that are not cached.
	if len(missTexts) > 0 {
		vectors, err := c.embedRemote(ctx, missTexts)
		if err != nil {
			return nil, err
		}

		for i, key := range missKeys {
			for _, idx := range missIndices[key] {
				resultVectors[idx] = vectors[i]
			}
		}
		c.writeCache(ctx, missKeys, vectors)
	}

	return resultVectors, nil
}

// readCache looks the keys up in one query. A failing cache is a slower answer,
// not a failed one, so an error is logged and every text counts as a miss.
func (c *HTTPEmbedder) readCache(ctx context.Context, keys []string) map[string][]float32 {
	rows, err := c.db.Queries.GetEmbeddingCacheByHashes(ctx, keys)
	if err != nil {
		slog.Warn("Failed to read the embedding cache", "error", err)
		return nil
	}

	found := make(map[string][]float32, len(rows))
	for _, row := range rows {
		found[row.ContentHash] = row.Embedding.Slice()
	}
	return found
}

// embedRemote asks the embedding service for the vectors of texts and refuses an
// answer from the wrong model.
func (c *HTTPEmbedder) embedRemote(ctx context.Context, texts []string) ([][]float32, error) {
	jsonBody, _ := json.Marshal(EmbedRequest{Texts: texts})

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

	if len(embedResp.Vectors) != len(texts) {
		return nil, fmt.Errorf("returned vectors count %d does not match request %d", len(embedResp.Vectors), len(texts))
	}
	return embedResp.Vectors, nil
}

// writeCache stores the new vectors with one statement. The answer is already in
// hand, so a failure is only logged.
func (c *HTTPEmbedder) writeCache(ctx context.Context, keys []string, vectors [][]float32) {
	literals := make([]string, len(vectors))
	for i, vec := range vectors {
		literals[i] = pgvector.NewVector(vec).String()
	}

	if err := c.db.Queries.CreateEmbeddingCacheBatch(ctx, db.CreateEmbeddingCacheBatchParams{
		Hashes:     keys,
		Embeddings: literals,
	}); err != nil {
		slog.Warn("Failed to cache embeddings", "error", err, "entries", len(keys))
	}
}
