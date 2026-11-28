package agent

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

// The Telegram message of 2026-11-19, word for word.
const booksAndNews = "Сега според документите и новините за anthropic  какъв съвет би ми дала -да купувам или продавам"

func TestAsksBooksAndWeb(t *testing.T) {
	for _, m := range []string{
		booksAndNews,
		"какво казват книгите ми и какво пише в интернет за стоицизма? според книгите и според интернет",
		"според библиотеката и новините, как е икономиката?",
		"what do my books and the news say about inflation? according to my books",
	} {
		assert.True(t, asksBooksAndWeb(m), m)
	}
	for _, m := range []string{
		"Имах предвид базата с документи по икономика ,как биха оценили ситуацията", // the books only
		"Какви са днешните водещи световни новини",                                   // the news only
		"потърси в интернет книги за стоицизма",                                     // books as what to find, not a source
		"добави в библиотеката тази статия от интернет: https://a.example/x",       // a library add
		"кажи ми според книгите как да го преструктурирам",                          // the books only
	} {
		assert.False(t, asksBooksAndWeb(m), m)
	}
}

func TestTheBooksAreSearchedFirstWhenBothAreAsked(t *testing.T) {
	tools := toolDefinitions()
	require.NotNil(t, onlyTool(tools, "web_search"), "the definitions have the internet")
	assert.Equal(t, searchDocumentsTool, forcedFirstTool(tools, booksAndNews))
	assert.Equal(t, "web_search", forcedFirstTool(tools, "Какви са днешните водещи световни новини"), "the news alone, as before")
	assert.Equal(t, "web_search", forcedFirstTool(onlyTool(tools, "web_search"), booksAndNews), "without the books on offer only the internet is left")
}

// The whole turn: the books, then the internet, both required, and the note before the answer.
func TestTheTurnSearchesTheBooksThenTheInternetAndIsToldToSayWhichIsWhich(t *testing.T) {
	local := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", searchDocumentsTool, `{"query": "IPO risk"}`)},
		{toolCallChunk(0, "c2", "web_search", `{"query": "Anthropic IPO news"}`)},
		{textChunk("От книгите … От новините …")},
	}}
	o := &Orchestrator{llmClient: local, tools: &fakeToolRegistry{result: "ok"}}

	content, _, err := ask(t, o, context.Background(), booksAndNews)

	require.NoError(t, err)
	assert.Equal(t, "От книгите … От новините …", content)
	require.Len(t, local.Requests, 3)
	assert.Equal(t, []string{searchDocumentsTool}, toolNames(local.Requests[0].Tools))
	assert.Equal(t, "required", local.Requests[0].ToolChoice)
	assert.Equal(t, []string{"web_search"}, toolNames(local.Requests[1].Tools), "the internet in the round after the books")
	assert.Equal(t, "required", local.Requests[1].ToolChoice)
	last := local.Requests[2].Messages
	var webResult string
	for _, m := range last {
		if m.Role == openai.ChatMessageRoleTool && m.ToolCallID == "c2" {
			webResult = m.Content
		}
	}
	assert.True(t, strings.HasSuffix(webResult, booksAndWebNote), "the note ends the result of the internet:\n%s", webResult)
	for _, m := range last {
		if m.Role == openai.ChatMessageRoleTool && m.ToolCallID == "c1" {
			assert.NotContains(t, m.Content, booksAndWebNote, "not after the books")
		}
	}
}

func TestTheBooksAndTheInternetStayAtHomeUnlessTheButtonSaysCloud(t *testing.T) {
	t.Run("home, though a question to the library goes to the cloud", func(t *testing.T) {
		local, paid := say("локално"), say("облак")
		before := routeCount("local", "books_web_local")

		content, by, err := ask(t, routed(local, paid, true), context.Background(), booksAndNews)

		require.NoError(t, err)
		assert.Equal(t, "локално", content)
		assert.Equal(t, "local", by.Provider)
		assert.Empty(t, paid.Requests)
		assert.EqualValues(t, 1, routeCount("local", "books_web_local")-before)
	})
	t.Run("the button of the cloud", func(t *testing.T) {
		local, paid := say("локално"), say("облак")

		content, by, err := ask(t, routed(local, paid, true), WithLLMChoice(context.Background(), yes()), booksAndNews)

		require.NoError(t, err)
		assert.Equal(t, "облак", content)
		assert.Equal(t, "deepseek", by.Provider)
		assert.Empty(t, local.Requests)
	})
}

func TestTheHelpListNamesTheBooksAndTheNews(t *testing.T) {
	assert.Contains(t, (&Orchestrator{}).helpText(), "Според книгите и новините")
}
