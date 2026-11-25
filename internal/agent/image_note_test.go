package agent

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/bills"
	"go-monolith/internal/llm"
)

func TestAsksAboutImages(t *testing.T) {
	// The owner's of 2026-11-24, 21:17-21:22.
	for _, m := range []string{
		"Ами имаш инструмент за четене на фактури който работи с OCR и направих снимка на един справочник за да проверя как ще го разпознаеш,сега се чудя дали ти е нужно да разпознаваш снимки",
		"Ами от познанията си по код и ИИ обясни ми как виждаш снимката от камерата ,какъв формат и т.н",
		"Трябва ли модела да има vision или може да се справи и само с OCR",
		"А можеш ли да прочетеш какво пише на снимката",
		"how do you see a photo?",
	} {
		assert.True(t, asksAboutImages(m), m)
	}
	for _, m := range []string{
		"Какво казва Сенека за гнева?",
		"Снимах залеза днес.", // a photo, no question how it is seen
		"колко платих за ток",
		"",
	} {
		assert.False(t, asksAboutImages(m), m)
	}
}

// The note reaches the model on that request only, and the checks of the turn read the person's words.
func TestTheNoteAboutPicturesGoesToTheModelOnly(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("Не виждам снимки — получавам само текста от OCR.")}}}
	o := &Orchestrator{llmClient: mock, tools: &fakeToolRegistry{}}
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "обясни ми как виждаш снимката от камерата"}}

	_, _, err := o.generateResponse(context.Background(), history, toolDefinitions(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 1)
	last := mock.Requests[0].Messages[len(mock.Requests[0].Messages)-1].Content
	assert.True(t, strings.HasSuffix(last, imageNote))
	assert.Equal(t, "обясни ми как виждаш снимката от камерата", history[0].Content, "the note is not stored")
	assert.Equal(t, "обясни ми как виждаш снимката от камерата", lastUserContent(mock.Requests[0].Messages), "the checks see the person's words")
}

func TestTheHelpSaysThatPicturesAreReadAsText(t *testing.T) {
	o := (&Orchestrator{}).WithHelpAnswer()
	o.bills = nil
	assert.NotContains(t, o.helpText(), "OCR", "with the bills off the line is not there")
	o.bills = &bills.Service{}
	assert.Contains(t, o.helpText(), "Самата картина не я виждам: снимката се чете с OCR")
	assert.Contains(t, o.helpText(), "прикачвам прочетения текст към разговора")
}
