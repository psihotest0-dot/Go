package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The memories of a planned answer come under it in italics; their Markdown marks stay text.
func TestTheRememberedLineIsItalicAndItsMemoriesStayText(t *testing.T) {
	assert.Equal(t, "Почини си.", withRemembered("Почини си.", nil), "no plan, nothing added")

	text := withRemembered("Почини си.\n", []string{"почива на дивана", "обича *рок* и C_PLUS"})
	assert.Equal(t, "Почини си.\n\n<i>Лили си спомни: почива на дивана; обича *рок* и C_PLUS</i>", markdownToHTML(text), "a blank line between the answer and the line")
}

// The bot sends the line with the answer of a planned turn.
func TestAPlannedAnswerIsSentWithTheRememberedLine(t *testing.T) {
	f := newFixture(42)
	f.processor.answer = "Почини си."
	f.processor.remembered = []string{"почива на дивана"}

	f.bot.handleMessage(context.Background(), privateMessage(42, "уморен съм"))

	require.Len(t, f.sender.messages, 1)
	assert.Contains(t, f.sender.messages[0].Text, "<i>Лили си спомни: почива на дивана</i>")
}
