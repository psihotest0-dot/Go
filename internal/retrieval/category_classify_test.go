package retrieval

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go-monolith/internal/config"
)

// axisManager has one orthogonal centroid per category, so a vector that points
// along an axis votes for exactly that category.
func axisManager() (*CategoryManager, map[string][]float32) {
	cm := &CategoryManager{centroids: map[string][]float32{}}
	axes := map[string][]float32{}
	for i, cat := range config.FileCategories {
		v := make([]float32, config.VectorDimension)
		v[i] = 1
		cm.centroids[cat] = v
		axes[cat] = v
	}
	return cm, axes
}

// votesFor builds the vectors of a file whose chunks vote as given.
func votesFor(axes map[string][]float32, votes map[string]int) [][]float32 {
	var out [][]float32
	for _, cat := range config.FileCategories { // fixed order: the test does not depend on a map
		for i := 0; i < votes[cat]; i++ {
			out = append(out, axes[cat])
		}
	}
	return out
}

func TestClassifyAcceptsAClearMajority(t *testing.T) {
	cm, axes := axisManager()

	got := cm.Classify(votesFor(axes, map[string]int{"economics": 20, "law": 6, "psychology": 4}))

	assert.Equal(t, "economics", got.Category)
	assert.Equal(t, "economics", got.Leader)
	assert.Equal(t, 20, got.LeaderVotes)
	assert.Equal(t, 30, got.Sampled)
}

// The first 30 chunks of a book of principles gave psychology 12, law 11 and
// programming 3 (and 4 for others): nobody has half of the votes.
func TestClassifyFallsBackToGeneralWhenVotesAreSplit(t *testing.T) {
	cm, axes := axisManager()

	got := cm.Classify(votesFor(axes, map[string]int{"psychology": 12, "law": 11, "programming": 3, "medicine": 2, "literature": 2}))

	assert.Equal(t, config.DefaultFileCategory, got.Category)
	assert.Equal(t, "psychology", got.Leader, "what led is still reported")
	assert.Equal(t, 12, got.LeaderVotes)
}

func TestClassifyNeedsMoreThanHalfAgainstARunnerUp(t *testing.T) {
	cm, axes := axisManager()

	// Exactly half is enough ...
	assert.Equal(t, "law", cm.Classify(votesFor(axes, map[string]int{"law": 15, "psychology": 10, "medicine": 5})).Category)
	// ... but not when another category has as many.
	assert.Equal(t, config.DefaultFileCategory, cm.Classify(votesFor(axes, map[string]int{"law": 15, "psychology": 15})).Category)
}

func TestClassifyOnlyTheFirstChunksVote(t *testing.T) {
	cm, axes := axisManager()
	vectors := votesFor(axes, map[string]int{"economics": 30})
	// 200 chunks of another subject after the sample must not change the result.
	for i := 0; i < 200; i++ {
		vectors = append(vectors, axes["law"])
	}

	got := cm.Classify(vectors)

	assert.Equal(t, "economics", got.Category)
	assert.Equal(t, 30, got.Sampled)
}

func TestClassifySmallAndEmptyInput(t *testing.T) {
	cm, axes := axisManager()

	few := cm.Classify(votesFor(axes, map[string]int{"medicine": 2}))
	assert.Equal(t, "medicine", few.Category)
	assert.Equal(t, 2, few.Sampled)

	none := cm.Classify(nil)
	assert.Equal(t, config.DefaultFileCategory, none.Category)
	assert.Zero(t, none.Sampled)

	// A chunk without a vector does not vote, and does not use up the sample.
	skipped := cm.Classify([][]float32{nil, {}, axes["law"], axes["law"]})
	assert.Equal(t, "law", skipped.Category)
	assert.Equal(t, 2, skipped.Sampled)
}

// A chunk that is not near any category votes for "general".
func TestClassifyChunkFarFromEveryCentroidVotesGeneral(t *testing.T) {
	cm, axes := axisManager()
	far := make([]float32, config.VectorDimension)
	far[config.VectorDimension-1] = 1 // orthogonal to every axis: similarity 0

	got := cm.Classify(votesFor(axes, map[string]int{"law": 4}))
	assert.Equal(t, "law", got.Category)

	got = cm.Classify([][]float32{far, far, far, axes["law"]})
	assert.Equal(t, config.DefaultFileCategory, got.Category)
	assert.Equal(t, config.DefaultFileCategory, got.Leader)
	assert.Equal(t, 3, got.LeaderVotes)
}

// The same vectors always give the same answer (the old code walked a map).
func TestClassifyIsDeterministic(t *testing.T) {
	cm, axes := axisManager()
	vectors := votesFor(axes, map[string]int{"law": 10, "psychology": 10, "economics": 10})

	first := cm.Classify(vectors)
	for i := 0; i < 50; i++ {
		assert.Equal(t, first, cm.Classify(vectors))
	}
	assert.Equal(t, config.DefaultFileCategory, first.Category)
}
