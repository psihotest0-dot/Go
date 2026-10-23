package retrieval

import (
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	assert.InDelta(t, float32(1.0), cosineSimilarity(a, b), 0.0001)

	c := []float32{0, 1, 0}
	assert.InDelta(t, float32(0.0), cosineSimilarity(a, c), 0.0001)

	d := []float32{0, 0, 0}
	assert.Equal(t, float32(0.0), cosineSimilarity(a, d))
}
