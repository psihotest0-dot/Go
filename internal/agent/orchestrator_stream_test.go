package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamFilterKeepsBufferBoundedOnUnterminatedTag(t *testing.T) {
	filter := &streamFilter{}
	input := strings.Repeat("<", 512) + "not a tag"

	out := filter.Process(input)

	require.LessOrEqual(t, len(filter.buffer), streamFilterMaxBuffered, "buffer must stay bounded")
	require.Equal(t, input, out+filter.Flush())
	require.Empty(t, filter.buffer)
}

func TestStreamFilterResetReleasesBuffer(t *testing.T) {
	filter := &streamFilter{}

	_ = filter.Process("<thi")
	require.NotEmpty(t, filter.buffer)

	filter.Reset()
	require.Empty(t, filter.buffer)
	require.False(t, filter.inThink)

	_ = filter.Process("<think>hidden reasoning")
	require.True(t, filter.inThink)
	require.Empty(t, filter.Process("more hidden reasoning"))

	filter.Reset()
	require.False(t, filter.inThink)
	require.Empty(t, filter.buffer)
}
