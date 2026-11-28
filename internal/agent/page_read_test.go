package agent

import (
	"context"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

const shownPage = "ето статия: https://libertystreeteconomics.newyorkfed.org/?p=43261"

func TestAsksAboutPage(t *testing.T) {
	for _, m := range []string{
		shownPage, // the live check of 2026-11-28
		"виж това https://go.dev/blog/go1.22",
		"https://go.dev/blog/go1.22",
		"какво мислиш за https://go.dev/blog/go1.22 ?",
		"прочети https://arxiv.org/pdf/2603.11838 и ми кажи за какво е",
	} {
		assert.True(t, asksAboutPage(m), m)
	}
	for _, m := range []string{
		"добави в библиотеката: https://arxiv.org/pdf/2603.11838",                  // add_to_library
		"запиши цялата статия във файл: https://go.dev/blog/go1.22",               // save_page
		"ето източниците: https://a.example/x.pdf https://b.example/y",            // two links
		"запомни този линк https://go.dev/blog/go1.22",                            // no word for a look
		"за всеки линк ми кажи какво е и колко е голям https://go.dev/blog/go1.22", // the link check
		"виж това",        // no link
		"здравей, как си", // nothing of the kind
	} {
		assert.False(t, asksAboutPage(m), m)
	}
}

func TestAShownPageIsReadFirstAndStaysWhereTheTurnIs(t *testing.T) {
	tools := toolDefinitions()
	require.NotNil(t, onlyTool(tools, "web_fetch"))
	assert.Equal(t, "web_fetch", forcedFirstTool(tools, shownPage))
	assert.Empty(t, forcedFirstTool(onlyTool(tools, "web_search"), shownPage), "without the reader nothing is forced")

	local := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", "web_fetch", `{"url": "https://libertystreeteconomics.newyorkfed.org/?p=43261"}`)},
		{textChunk("Статията е за нощния дрейф.")},
	}}
	paid := say("облак")
	content, by, err := ask(t, routed(local, paid, true), context.Background(), shownPage)

	require.NoError(t, err)
	assert.Equal(t, "Статията е за нощния дрейф.", content)
	assert.Equal(t, "local", by.Provider, "reading a page is no question to the library: it does not go to the cloud")
	assert.Empty(t, paid.Requests)
	require.NotEmpty(t, local.Requests)
	assert.Equal(t, []string{"web_fetch"}, toolNames(local.Requests[0].Tools))
	assert.Equal(t, "required", local.Requests[0].ToolChoice)
}
