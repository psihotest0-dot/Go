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

	"go-monolith/internal/db"

	"github.com/pgvector/pgvector-go"
)

type EmbedderClient struct {
	baseURL string
	db      *db.Database
	client  *http.Client
}

func NewEmbedderClient(database *db.Database) *EmbedderClient {
	baseURL := os.Getenv("EMBEDDER_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8000"
	}
	return &EmbedderClient{
		baseURL: baseURL,
		db:      database,
		client:  &http.Client{},
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

func (c *EmbedderClient) EmbedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	// 1. Chunk texts
	var allChunks []string
	for _, text := range texts {
		chunks := chunkText(text, 100) // chunk to ~100 words
		allChunks = append(allChunks, chunks...)
	}

	if len(allChunks) == 0 {
		return nil, nil
	}

	resultVectors := make([][]float32, len(allChunks))
	textsToEmbed := []string{}
	indicesToEmbed := []int{}

	// 2. Check cache for each chunk
	for i, chunk := range allChunks {
		h := hashContent(chunk)
		cached, err := c.db.Queries.GetEmbeddingCacheByHash(ctx, h)
		if err == nil {
			// Found in cache
			// the embedding is returned as pgvector, we need to convert it
			// However in our DB struct it's interface{}, we need to decode it
			// Or if we use pgvector-go we can handle it.
			// For this simplified logic we will just re-embed if we can't parse easily.
			// Let's properly handle pgvector using pgvector-go.

			// But for now since it's an interface{} we might need to rely on string parsing if not careful.
			// Let's actually parse it.
			valStr := ""
			switch v := cached.Embedding.(type) {
			case string:
				valStr = v
			case []byte:
				valStr = string(v)
			}

			vec := pgvector.NewVector(nil)
			if err := vec.Scan(valStr); err == nil {
				resultVectors[i] = vec.Slice()
				continue
			}
		}

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

		// 4. Update cache and result
		for i, vec := range embedResp.Vectors {
			origIdx := indicesToEmbed[i]
			resultVectors[origIdx] = vec

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

	return resultVectors, nil
}
