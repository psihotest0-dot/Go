package agent

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

func TestAsksForTheWholeList(t *testing.T) {
	for _, m := range []string{
		"Това ли са всички за днес?",
		"Искам да ми изброил всички мачове от лигата за днес",
		"Общо са 9 ,защо ми показвал само 4 .провери пак",
		"дай ми пълния списък",
		"а останалите?",
		"липсват два",
		"is that all of them?",
	} {
		assert.True(t, asksForTheWholeList(m), m)
	}
	for _, m := range []string{
		"Мерси",
		"кой спечели?",
		"разкажи ми повече за мача на Барселона",
		"благодаря ти",
		"",
		// A long message is a new request, whatever words it has.
		"Сега искам нещо друго: напиши ми подробно писмо до управителя на сградата, в което да обясниш, че всички съседи от третия етаж " +
			"имат проблем с отоплението от седмица насам и че очакваме да дойде техник до края на месеца, иначе ще търсим други решения.",
	} {
		assert.False(t, asksForTheWholeList(m), m)
	}
}

// matchesHistory is the Telegram turn of 2026-11-25: an answer from a web search, a short answer
// without one, then the person's message.
func matchesHistory(message string) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "Провери в интернет какви мачове има днес от шампионската лига"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Днес има няколко мача: …" +
			historyToolNote(`web_search "Champions League matches today November 25 2026": results read (4.9 KB)`)},
		{Role: openai.ChatMessageRoleUser, Content: "Това ли са всички за днес?"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Да, това са основните мачове."},
		{Role: openai.ChatMessageRoleUser, Content: message},
	}
}

func TestRecentAnswerSearchedWeb(t *testing.T) {
	assert.True(t, recentAnswerSearchedWeb(matchesHistory("изброй всички")), "two answers back")
	assert.False(t, recentAnswerSearchedWeb([]openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "колко е часът?"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Не знам точно."},
		{Role: openai.ChatMessageRoleUser, Content: "изброй всички"},
	}), "no search")
	long := matchesHistory("изброй всички")
	for range webFollowupAnswers {
		long = append(long[:len(long)-1], openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "друго"},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "изброй всички"})
	}
	assert.False(t, recentAnswerSearchedWeb(long), "the search is too far back")
	// The model cannot write the record itself: an answer that only says "web_search" is not one.
	assert.False(t, recentAnswerSearchedWeb([]openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleAssistant, Content: `I used web_search "x" before.`},
		{Role: openai.ChatMessageRoleUser, Content: "изброй всички"},
	}))
}

// 21:26: "Искам да ми изброиш всички мачове" - the first round must search again.
func TestAskingForTheWholeListAfterAWebAnswerSearchesAgain(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "web_search", `{"query":"Champions League 25 November 2026 all matches"}`)},
		{textChunk("Ето деветте мача.")},
	}}
	reg := &fakeToolRegistry{result: map[string]any{"results": []any{}}}
	o := newGroupOrchestrator(mock, reg)
	ctx, _ := withTurn(t.Context(), "s", false)

	content, executed, err := o.generateResponse(ctx, matchesHistory("Искам да ми изброил всички мачове от лигата за днес"), o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Ето деветте мача.", content)
	assert.Equal(t, []string{"web_search"}, toolCallNames(executed))
	require.NotEmpty(t, mock.Requests)
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
	assert.Equal(t, []string{"web_search"}, toolNames(mock.Requests[0].Tools))
}

// Without an answer from the internet before it, "всички" is left to the model.
func TestAskingForAllWithoutAWebAnswerIsLeftToTheModel(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("Ето всички.")}}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})
	ctx, _ := withTurn(t.Context(), "s", false)
	_, _, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "кажи ми всички планети"},
	}, o.turnTools(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, mock.Requests)
	assert.Nil(t, mock.Requests[0].ToolChoice)
}
