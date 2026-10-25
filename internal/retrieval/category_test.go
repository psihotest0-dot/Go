package retrieval_test



import (
	"context"
	"fmt"
	"go-monolith/internal/config"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func TestCategoryManager_Initialize(t *testing.T) {
	embedder := &llm.MockEmbedder{}
	cm, err := retrieval.NewCategoryManager(context.Background(), embedder)
	require.NoError(t, err)
	assert.NotNil(t, cm)

	// Check if centroids are created
	centroids := cm.GetAllCentroids()
	assert.NotEmpty(t, centroids)

	centroid, ok := cm.GetCentroid("medicine")
	assert.True(t, ok)
	assert.Len(t, centroid, config.VectorDimension)
}

func TestCategoryManager_Initialize_EmbedderError(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{Err: fmt.Errorf("embed error")}
	_, err := retrieval.NewCategoryManager(context.Background(), embedder)
	require.ErrorContains(t, err, "failed to initialize category centroids")
}

func TestCategoryManager_Initialize_DimensionError(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{Dim: 128}
	_, err := retrieval.NewCategoryManager(context.Background(), embedder)
	require.ErrorContains(t, err, "expected vector dimension "+fmt.Sprint(config.VectorDimension))
}

func TestCategoryManager_Initialize_InvalidDimension(t *testing.T) {
	embedder := &llm.MockEmbedderWithConfig{Dim: 128} // Return dim 128
	_, err := retrieval.NewCategoryManager(context.Background(), embedder)
	require.ErrorContains(t, err, "expected vector dimension "+fmt.Sprint(config.VectorDimension))
}

func TestCategoryManager_Initialize_MismatchVectorsCount(t *testing.T) {
	// Custom mock to return fewer vectors
	embedder := &CustomEmbedder{}
	_, err := retrieval.NewCategoryManager(context.Background(), embedder)
	require.ErrorContains(t, err, "expected 42 vectors, got 1") // 3 * 7 = 21
}

type CustomEmbedder struct{}

func (c *CustomEmbedder) EmbedTexts(ctx context.Context, texts []string, isQuery bool) ([][]float32, error) {
	return [][]float32{make([]float32, config.VectorDimension)}, nil
}
