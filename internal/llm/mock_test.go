package llm

import (
	"context"
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestMockEmbedder_EmptyTexts(t *testing.T) {
	embedder := &MockEmbedder{}
	vectors, err := embedder.EmbedTexts(context.Background(), []string{})
	assert.NoError(t, err)
	assert.Nil(t, vectors)
}
