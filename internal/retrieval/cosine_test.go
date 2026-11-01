package retrieval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/llm"
)

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	assert.InDelta(t, float32(1.0), CosineSimilarity(a, b), 0.0001)

	c := []float32{0, 1, 0}
	assert.InDelta(t, float32(0.0), CosineSimilarity(a, c), 0.0001)

	d := []float32{0, 0, 0}
	assert.Equal(t, float32(0.0), CosineSimilarity(a, d))
}

// The categories a file can have are listed once, in config. The classifier must
// know exactly those: a category it cannot choose is dead, and one it chooses
// that the category endpoint refuses could never be set back by hand.
func TestCategoryCentroidsMatchTheConfiguredCategories(t *testing.T) {
	cm, err := NewCategoryManager(context.Background(), &llm.MockEmbedder{})
	require.NoError(t, err)

	got := make([]string, 0, len(cm.GetAllCentroids()))
	for category := range cm.GetAllCentroids() {
		got = append(got, category)
	}
	assert.ElementsMatch(t, config.FileCategories, got)
}
