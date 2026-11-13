package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

func runOneTool(t *testing.T, tools *fakeToolRegistry, question, tool string, budget int) string {
	t.Helper()
	o := newGenerateOrchestrator(&llm.MockLLM{}, tools)
	messages := o.executeToolCalls(WithCurrentQuery(context.Background(), question), []openai.ToolCall{
		{ID: "call_1", Function: openai.FunctionCall{Name: tool, Arguments: `{"query":"x"}`}},
	}, budget)
	require.Len(t, messages, 1)
	return messages[0].Content
}

func TestASearchForAPassageEndsWithTheQuoteNote(t *testing.T) {
	tools := &fakeToolRegistry{result: map[string]interface{}{"status": "success", "results": []string{"откъс"}}}

	content := runOneTool(t, tools, "цитирай Марк Аврелий за смъртта", searchDocumentsTool, defaultToolResultTokens)

	assert.True(t, strings.HasSuffix(content, toolNoteSeparator+passageQuoteNote), content)
	assert.True(t, strings.HasPrefix(content, `{"results":["откъс"],"status":"success"}`), "the result itself comes first, unchanged")
}

func TestTheQuoteNoteIsOnlyForASearchForAPassage(t *testing.T) {
	ok := map[string]interface{}{"status": "success"}

	cases := []struct{ name, question, tool string }{
		{"a question to the books", "Какво казва Марк Аврелий за смъртта?", searchDocumentsTool},
		{"small talk", "дай ми съвет", searchDocumentsTool},
		{"another tool", "цитирай Марк Аврелий за смъртта", "search_memories"},
		{"the library list", "цитирай Марк Аврелий за смъртта", listDocumentsTool},
	}
	for _, c := range cases {
		content := runOneTool(t, &fakeToolRegistry{result: ok}, c.question, c.tool, defaultToolResultTokens)
		assert.NotContains(t, content, passageQuoteNote, c.name)
	}

	// A failed search has no passages to quote from.
	content := runOneTool(t, &fakeToolRegistry{err: errors.New("boom")}, "цитирай Марк Аврелий за смъртта", searchDocumentsTool, defaultToolResultTokens)
	assert.NotContains(t, content, passageQuoteNote)
	assert.Contains(t, content, "boom")
}

func TestACutSearchResultKeepsTheQuoteNote(t *testing.T) {
	tools := &fakeToolRegistry{result: strings.Repeat("а", 100_000)}

	content := runOneTool(t, tools, "дай ми абзац от Граф Монте Кристо", searchDocumentsTool, 500)

	assert.LessOrEqual(t, estimateTokens(content), 500, "the note counts against the budget")
	assert.Contains(t, content, toolResultTruncatedMarker)
	assert.True(t, strings.HasSuffix(content, passageQuoteNote), "the cut falls on the passages, not on the note")

	// A result that fits the budget alone but not with the note is cut too.
	almost := &fakeToolRegistry{result: strings.Repeat("a", 490*bytesPerToken)}
	content = runOneTool(t, almost, "дай ми абзац от Граф Монте Кристо", searchDocumentsTool, 500)
	assert.LessOrEqual(t, estimateTokens(content), 500)
	assert.True(t, strings.HasSuffix(content, passageQuoteNote))
}

func TestNoRoomForTheResultLeavesTheNoteOut(t *testing.T) {
	tools := &fakeToolRegistry{result: strings.Repeat("а", 10_000)}

	content := runOneTool(t, tools, "дай ми абзац от Граф Монте Кристо", searchDocumentsTool, 0)

	assert.Equal(t, toolResultOmittedNotice, content)
}
