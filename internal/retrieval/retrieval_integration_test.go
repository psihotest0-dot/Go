package retrieval_test

import (
	"context"
	"os"
	"testing"

	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func TestRetrieval_Integration(t *testing.T) {
	connString := dbtest.ConnString(t)
	embedderURL := os.Getenv("EMBEDDER_URL")

	if embedderURL == "" {
		t.Skip("Пропускане: няма embedder")
	}

	// db.RunMigrations resolves "migrations" relative to the working directory,
	// which is the package directory under `go test`.
	t.Chdir("../..")

	err := db.RunMigrations(connString)
	require.NoError(t, err)

	ctx := context.Background()
	d, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	defer d.Close()

	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)

	// Only this test's own rows are removed; a bare `DELETE FROM users` would
	// cascade through the whole library (docs/incident-2026-10-30/REPORT.md).
	defer func() {
		_, _ = d.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	}()

	// Since we mock embedder with a health check, we need to pass the real HTTPEmbedder if we want,
	// but the requirement is to use MockEmbedder or real depending on availability.
	// We'll use HTTPEmbedder if the URL is provided.
	embedder := llm.NewHTTPEmbedder(d)
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	content := "The capital of France is Paris."
	mem, err := svc.CreateMemory(ctx, user.ID, content, 5, "active", pqtype.NullRawMessage{})
	require.NoError(t, err)
	assert.Equal(t, content, mem.Content)

	query := "What is the capital of France?"
	results, err := svc.SearchMemories(ctx, user.ID, query, 1)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, content, results[0].Content)
}

func TestRetrieval_HybridSearch_Bulgarian(t *testing.T) {
	connString := dbtest.ConnString(t)
	embedderURL := os.Getenv("EMBEDDER_URL")

	if embedderURL == "" {
		t.Skip("Пропускане: няма embedder")
	}

	// db.RunMigrations resolves "migrations" relative to the working directory,
	// which is the package directory under `go test`.
	t.Chdir("../..")

	err := db.RunMigrations(connString)
	require.NoError(t, err)

	ctx := context.Background()
	d, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	defer d.Close()

	user, err := d.Queries.CreateUser(ctx)
	require.NoError(t, err)

	// Only this test's own rows are removed; a bare `DELETE FROM users` would
	// cascade through the whole library (docs/incident-2026-10-30/REPORT.md).
	defer func() {
		_, _ = d.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	}()

	embedder := llm.NewHTTPEmbedder(d)
	svc := retrieval.NewRetrievalService(d, embedder, nil)

	facts := []string{
		"Кучето лае през нощта.",
		"Котката спи на дивана.",
		"Слънцето изгрява от изток.",
		"Земята се върти около слънцето.",
		"Водата кипи при 100 градуса Целзий.",
		"Мария обича да чете фантастика.",
		"Иван играе футбол всяка събота.",
		"България се намира на Балканския полуостров.",
		"Столицата на България е София.",
		"Връх Мусала е най-високият на Балканите.",
		"Река Дунав е естествена граница.",
		"Черно море е на изток.",
		"Компютрите използват двоичен код.",
		"Интернетът е глобална мрежа.",
		"Пицата е италианско ястие.",
		"Япония е страна на изгряващото слънце.",
		"Кенгуруто е символ на Австралия.",
		"Ейфеловата кула е в Париж.",
		"Ню Йорк е известен град.",
		"Обичам да пия кафе сутрин.",
	}

	for _, f := range facts {
		_, createErr := svc.CreateMemory(ctx, user.ID, f, 5, "active", pqtype.NullRawMessage{})
		require.NoError(t, createErr)
	}

	// 1. Orthographic mistake + grammar + semantic query
	// Target: "Връх Мусала е най-високият на Балканите."
	// Query: "Кой е наи-високия врух на балканите?"
	query := "Кой е наи-високия врух на балканите?"
	results, err := svc.SearchMemories(ctx, user.ID, query, 3)
	require.NoError(t, err)

	found := false
	for _, r := range results {
		if r.Content == "Връх Мусала е най-високият на Балканите." {
			found = true
			break
		}
	}
	assert.True(t, found, "Expected to find target fact in top 3 results")
}
