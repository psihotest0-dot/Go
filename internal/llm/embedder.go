package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go-monolith/internal/db"
	"go-monolith/metrics"

	"github.com/pgvector/pgvector-go"
)

// Embedder defines the interface for embedding texts
type Embedder interface {
	EmbedTexts(ctx context.Context, texts []string) ([][]float32, error)
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

func chunkText(text string, maxWords int) []string {
	words := strings.Fields(text)
	var chunks []string
	for i := 0; i < len(words); i += maxWords {
		end := i + maxWords
		if end > len(words) {
			end = len(words)
		}
		chunks = append(chunks, strings.Join(words[i:end], " "))
	}
	return chunks
}

func hashContent(content string) string {
	h := sha256.New()
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *HTTPEmbedder) EmbedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("embed").Observe(time.Since(start).Seconds())
	}()

	if len(texts) == 0 {
		return nil, nil
	}

	// 1. Chunk texts and keep track of which chunks belong to which original text
	var allChunks []string
	chunkCounts := make([]int, len(texts))
	for i, text := range texts {
		chunks := chunkText(text, 100) // chunk to ~100 words
		if len(chunks) == 0 {
			// Handle empty text by adding an empty string chunk so it still produces a vector (likely zeroes or padding)
			chunks = []string{""}
		}
		chunkCounts[i] = len(chunks)
		allChunks = append(allChunks, chunks...)
	}

	chunkVectors := make([][]float32, len(allChunks))
	textsToEmbed := []string{}
	indicesToEmbed := []int{}

	// 2. Check cache for each chunk
	for i, chunk := range allChunks {
		h := hashContent(chunk)
		cached, err := c.db.Queries.GetEmbeddingCacheByHash(ctx, h)
		if err == nil {
			// Found in cache
			metrics.CacheHits.Inc()
			chunkVectors[i] = cached.Embedding.Slice()
			continue
		}

		metrics.CacheMisses.Inc()
		textsToEmbed = append(textsToEmbed, chunk)
		indicesToEmbed = append(indicesToEmbed, i)
	}

	// 3. Request embeddings for non-cached chunks
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

		if embedResp.Dim != 384 {
			return nil, fmt.Errorf("unexpected dimension %d, expected 384", embedResp.Dim)
		}

		if len(embedResp.Vectors) != len(textsToEmbed) {
			return nil, fmt.Errorf("returned vectors count %d does not match request %d", len(embedResp.Vectors), len(textsToEmbed))
		}

		// Update cache and result
		for i, vec := range embedResp.Vectors {
			origIdx := indicesToEmbed[i]
			chunkVectors[origIdx] = vec

			h := hashContent(textsToEmbed[i])
			pgVec := pgvector.NewVector(vec)

			_, cacheErr := c.db.Queries.CreateEmbeddingCache(ctx, db.CreateEmbeddingCacheParams{
				ContentHash: h,
				Embedding:   pgVec,
			})
			if cacheErr != nil && !errors.Is(cacheErr, sql.ErrNoRows) {
				// We can ignore unique constraint errors if two concurrent processes insert it
			}
		}
	}

	// 4. Mean pooling: Average chunk vectors for each original text
	resultVectors := make([][]float32, len(texts))
	chunkIdx := 0
	for i, count := range chunkCounts {
		if count == 0 {
			continue // Should not happen due to empty text handling above
		}

		// Sum all vectors for this text
		sumVec := make([]float32, 384)
		for j := 0; j < count; j++ {
			vec := chunkVectors[chunkIdx]
			for k := 0; k < 384; k++ {
				if len(vec) > k {
					sumVec[k] += vec[k]
				}
			}
			chunkIdx++
		}

		// Average the sum vector
		for k := 0; k < 384; k++ {
			sumVec[k] /= float32(count)
		}

		resultVectors[i] = sumVec
	}

	return resultVectors, nil
}
