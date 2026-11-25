package agent

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
)

// extractionAnswer is a long answer that hands over an "extraction" of a source.
func extractionAnswer(lead string) string {
	var b strings.Builder
	b.WriteString(lead + "\n\n### Ключови точки\n")
	for i := 0; i < 8; i++ {
		b.WriteString("- Авторите показват, че нощната доходност е по-висока от дневната в тази извадка от акции и периоди.\n")
	}
	b.WriteString(strings.Repeat("Методологията използва регресии върху дневни данни. ", 10))
	return b.String()
}

func TestAnswerChecksHaveNamesOfTheirOwnAndTheFileChecksComeFirst(t *testing.T) {
	seen := map[string]bool{}
	for i, c := range answerChecks {
		assert.False(t, seen[c.name], "two checks named %s", c.name)
		seen[c.name] = true
		assert.NotEmpty(t, c.logMsg, c.name)
		assert.NotEmpty(t, c.op, c.name)
		if i > 0 && c.fileTools {
			assert.True(t, answerChecks[i-1].fileTools, "%s: the checks of the files come before the others", c.name)
		}
	}
	assert.Equal(t, []string{"claim", "stall", "ignored", "status", "lookup", "bill_claim", "extraction"}, func() []string {
		var names []string
		for _, c := range answerChecks {
			names = append(names, c.name)
		}
		return names
	}())
}

func TestTheFirstCheckThatFindsTheAnswerWrongIsTheOnlyLineUnderIt(t *testing.T) {
	withFiles := &Orchestrator{groups: []toolGroup{filesGroup()}}
	turn := &turnState{}
	ctx := context.Background()

	// A claimed write that is also an "extraction": the claim is the line, not both.
	answer := extractionAnswer("Записах всичко във файла notes.md.")
	note := withFiles.checkAnswer(ctx, answerFacts{content: answer, turn: turn, userMessage: "запиши го във файла notes.md", userNamedFile: true})
	assert.Equal(t, writeCorrection, note)

	// Without the file tools the checks of the files are not asked; the extraction still is.
	note = (&Orchestrator{}).checkAnswer(ctx, answerFacts{content: answer, turn: turn, userMessage: "запиши го във файла notes.md", userNamedFile: true})
	assert.Equal(t, extractionNotice, note)

	// An extraction after a tool opened the source is not one.
	opened := []openai.ToolCall{{Function: openai.FunctionCall{Name: "web_fetch"}}}
	assert.Empty(t, (&Orchestrator{}).checkAnswer(ctx, answerFacts{content: extractionAnswer("Ето го."), executed: opened, turn: turn}))

	// A promise for later with no tool, with the file tools on.
	note = withFiles.checkAnswer(ctx, answerFacts{content: "Ще започна веднага да пренаписвам файла. Когато завърша, ще ти кажа.", turn: turn, userMessage: "пренапиши файла"})
	assert.Equal(t, stallCorrection, note)

	// A plain answer passes them all.
	assert.Empty(t, withFiles.checkAnswer(ctx, answerFacts{content: "Добре, разбрах те.", turn: turn, userMessage: "как си"}))
}
