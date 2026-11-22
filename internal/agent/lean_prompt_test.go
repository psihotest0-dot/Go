package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/prompts"
)

// The parts the lean prompt leaves out (WithLeanPrompt), and the rules it must keep: the ones for
// the tools the model uses without being asked (memories, the library, the web for facts).
var (
	leanDropped = []string{"EXAMPLE TONE", "You have access to memory and knowledge base tools", "web_fetch(url):", "Пример:"}
	leanKept    = []string{"CHARACTER", "Call search_memories FIRST", "Call search_documents FIRST",
		"Facts about the world", "Call create_memory ONLY", "REPLY RULES", "BOUNDARIES"}
)

func TestTheLeanPromptLeavesOutOnlyWhatWasMeasured(t *testing.T) {
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	full, err := buildSystemPrompt(chatContext{})
	require.NoError(t, err)
	lean, err := buildSystemPrompt(chatContext{Lean: true})
	require.NoError(t, err)

	for _, part := range leanDropped {
		assert.Contains(t, full.Content, part, "the default prompt is today's")
		assert.NotContains(t, lean.Content, part)
	}
	for _, part := range leanKept {
		assert.Contains(t, lean.Content, part)
	}
	assert.Less(t, len(lean.Content), len(full.Content)*4/5)
}

// Lean, a conversation without an attached file is not offered read_attachment; one with a file
// is, and so is every conversation without the lean prompt.
func TestTheLeanPromptOffersReadAttachmentOnlyWithAFile(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel()).WithLeanPrompt()
	storeTurnMessages(t, e, "lean-1", [2]string{"user", "здравей"})
	storeTurnMessages(t, e, "lean-2", [2]string{"user", "виж файла"})
	e.attach(t, e.userID, "lean-2", "notes.go", "package notes\n")

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "lean-1", Content: "здравей"}, false)
	require.NoError(t, err)
	assert.NotContains(t, toolNames(prep.tools), readAttachmentTool)
	assert.Contains(t, toolNames(prep.tools), "search_documents")
	assert.NotContains(t, prep.messages[0].Content, "EXAMPLE TONE", "the lean system prompt")

	prep, err = o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "lean-2", Content: "виж файла"}, false)
	require.NoError(t, err)
	assert.Contains(t, toolNames(prep.tools), readAttachmentTool, "with an attached file")

	plain := orchestratorWith(t, e, answeringModel())
	prep, err = plain.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "lean-1", Content: "здравей"}, false)
	require.NoError(t, err)
	assert.Contains(t, toolNames(prep.tools), readAttachmentTool, "without the lean prompt nothing changes")
	assert.Contains(t, prep.messages[0].Content, "EXAMPLE TONE")
}
