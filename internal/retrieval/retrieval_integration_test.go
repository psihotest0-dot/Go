package retrieval_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func TestRetrieval_Integration(t *testing.T) {
	connString := os.Getenv("DATABASE_URL")
	embedderURL := os.Getenv("EMBEDDER_URL")

	if connString == "" || embedderURL == "" {
		t.Skip("Пропускане: няма база/embedder")
	}

	err := db.RunMigrations(connString)
	require.NoError(t, err)

	ctx := context.Background()
	d, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	defer d.Pool.Close()

	// Clear DB for test
	_, _ = d.Pool.Exec(ctx, "DELETE FROM memories")
	_, _ = d.Pool.Exec(ctx, "DELETE FROM users")

	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)

	// Since we mock embedder with a health check, we need to pass the real HTTPEmbedder if we want,
	// but the requirement is to use MockEmbedder or real depending on availability.
	// We'll use HTTPEmbedder if the URL is provided.
	embedder := llm.NewHTTPEmbedder(d)
	svc := retrieval.NewRetrievalService(d, embedder)

	content := "The capital of France is Paris."
	mem, err := svc.CreateMemory(ctx, user.ID, content, 5, "active", pqtype.NullRawMessage{})
	require.NoError(t, err)
	assert.Equal(t, content, mem.Content)

	query := "What is the capital of France?"
	results, err := svc.SearchSimilar(ctx, user.ID, query, 1)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, content, results[0].Content)
}
