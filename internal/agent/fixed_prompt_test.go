package agent

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/prompts"
)

// maxFixedPromptEstimate is the largest estimate (estimateTokens, four bytes to
// a token) the fixed part of a request may have: the system template rendered
// with an empty context, plus the tool definitions, as estimatePromptTokens
// counts them.
//
// It was calibrated on 2026-11-08. The estimate was 4 508 (template 3 330, tool
// definitions 1 172) while the model's own tokenizer counted 3 619 on 2026-11-07,
// so the estimate runs about 25 % above the real count (20-35 % in backend.log).
// fixedPromptTokens keeps only 131 tokens (3.6 %) of slack over that count, so
// the limit sits 3 % above today's estimate: past it the real count may already
// be over the reserve.
const maxFixedPromptEstimate = 4650

// fixedPromptTokens is measured by hand with the tokenizer of the model, and it
// once fell behind unnoticed (task 62: the V4 prompt had grown past the old
// 2 900 and nothing failed). This test is only a barrier against that silent
// drift. It does not replace the measurement.
func TestFixedPromptStaysWithinItsReserve(t *testing.T) {
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	system, err := buildSystemPrompt(chatContext{})
	require.NoError(t, err)
	template := estimateTokens(system.Content)
	require.Greater(t, template, 1000, "the real system template was rendered, not an empty one")

	tools := estimatePromptTokens(nil, toolDefinitions())
	require.Greater(t, tools, toolsTemplateOverheadTokens, "the real tool definitions were counted")

	total := estimatePromptTokens([]openai.ChatCompletionMessage{system}, toolDefinitions())

	assert.LessOrEqual(t, total, maxFixedPromptEstimate,
		"the system template (%d estimated tokens) and the tool definitions (%d) together are %d, over the %d "+
			"that fixedPromptTokens (%d) was measured against. Measure both with the model's own tokenizer "+
			"(see the comment at fixedPromptTokens), then update fixedPromptTokens and maxFixedPromptEstimate together",
		template, tools, total, maxFixedPromptEstimate, fixedPromptTokens)
}
