package agent

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

const sourcesMessage = "ето източниците: https://libertystreeteconomics.newyorkfed.org/?p=43261 https://arxiv.org/pdf/2603.11838"

func TestAClaimOfAnAddToTheLibrary(t *testing.T) {
	for _, answer := range []string{
		// The live check of 2026-11-28, 12:01 (it ends with a question, and says "веднага").
		"Ще ги добавя веднага към библиотеката ти, за да мога да ги анализирам по-задълбочено. Първият източник разглежда дългосрочните тенденции. Искаш ли да извлеча първо резюме от първия източник?",
		// The replay after a talk about inflation, 1 of 3.
		"Добавих тези източници към библиотеката ти, за да мога да се позова на тях по-нататък.",
		"Ще ги сваля в библиотеката ти и ще ти кажа как са кръстени.",
	} {
		assert.Equal(t, lookupAdd, lookupClaim(answer, sourcesMessage), answer)
	}
	for _, answer := range []string{
		"Ако искаш, ще ги добавя в библиотеката ти.",             // an offer with a condition
		"Мога да ги добавя в библиотеката. Да го направя ли?",    // a question, no promise
		"Ще ги добавя в библиотеката, щом кажеш. Да започна ли?", // a condition again
		"Добавих бележка във файла.",                             // no library
		"Интересни източници за нощния дрейф на акциите.",        // no claim at all
	} {
		assert.NotEqual(t, lookupAdd, lookupClaim(answer, sourcesMessage), answer)
	}
}

func addTurnTools(o *Orchestrator) []openai.Tool {
	return append(o.turnTools(), addToLibraryDefinition())
}

// "Добавих тези източници…" and no tool: asked again with add_to_library required; only the second
// answer is kept.
func TestAClaimedAddIsAskedAgainWithTheLibraryTool(t *testing.T) {
	const claim = "Добавих тези източници към библиотеката ти."
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk(claim)},
		{toolCallChunk(0, "call_1", addToLibraryTool, `{"urls":["https://arxiv.org/pdf/2603.11838"]}`)},
		{textChunk("Добавих 2603.pdf; чака обработка.")},
	}}
	reg := &fakeToolRegistry{result: map[string]any{"summary": "1 of 1 queued"}}

	content, executed, streamed := lookupTurn(t, mock, reg, addTurnTools, sourcesMessage)

	assert.Equal(t, "Добавих 2603.pdf; чака обработка.", content)
	assert.Equal(t, []string{addToLibraryTool}, toolCallNames(executed))
	assert.Equal(t, claim+lookupPlaces[lookupAdd].notice+"Добавих 2603.pdf; чака обработка.", streamed)
	require.Len(t, mock.Requests, 3)
	retry := mock.Requests[1]
	assert.Equal(t, "required", retry.ToolChoice)
	assert.Equal(t, []string{addToLibraryTool}, toolNames(retry.Tools))
	last := retry.Messages[len(retry.Messages)-1].Content
	assert.True(t, strings.HasPrefix(last, sourcesMessage))
	assert.Contains(t, last, addNote)
}

// No link of the person, so no add_to_library on offer: a line under the answer says nothing was added.
func TestAClaimedAddWithoutTheToolIsCorrected(t *testing.T) {
	const claim = "Добавих тези източници към библиотеката ти."
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(claim)}}}

	content, executed, _ := lookupTurn(t, mock, &fakeToolRegistry{}, allTurnTools, "добави ги в библиотеката")

	require.Len(t, mock.Requests, 1, "no retry")
	assert.Empty(t, executed)
	assert.Equal(t, claim+"\n\n"+addCorrection, content)
}
