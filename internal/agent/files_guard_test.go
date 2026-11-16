package agent

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
)

func TestAsksToRemoveAFile(t *testing.T) {
	asks := []string{
		// the real chat of 2026-11-14, where the answer was "изтрих файла"
		"не всичко е наред,опитай да изтриеш файла ,не се притеснявай това е тест",
		"Изтрий mood_log.txt",
		"изтрий бележката",
		"Махни файла със списъка",
		"Премахни папката с бележките",
		"Преименувай бележки.md на идеи.md",
		"Премести файла в друга папка",
		"Delete the file notes.txt",
		"please remove my notes",
		"rename plan.md",
	}
	for _, m := range asks {
		assert.True(t, asksToRemoveFile(m), m)
	}

	doesNotAsk := []string{
		"",
		"Запиши ми това във файл",
		"Прочети notes.txt",
		"Какво има в папката ти?",
		"Изтрий спомена за кучето",       // a memory, not a file
		"махни ми тази мисъл от главата", // a figure of speech
		"Не го изтривай!",                // no file
		"Добави сирене в list.txt",
		"Колко файла имаш?",
	}
	for _, m := range doesNotAsk {
		assert.False(t, asksToRemoveFile(m), m)
	}

	assert.False(t, asksToRemoveFile("изтрий файла "+strings.Repeat("и още много думи ", 40)), "a long message is a story or a task")
}

func TestAClaimOfAChangeThatDidNotHappen(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		wrote  bool
		want   string
		asked  bool
	}{
		{"a removal, the real answer", "Разбрах, изтрих файла `mood_log.txt` от папката ти. Тъй като е тест, няма проблем.", false, removalCorrection, false},
		{"a removal is never backed, even after a write", "Записах го и изтрих стария файл бележки.md.", true, removalCorrection, false},
		{"a write the real answer claimed", "Записах го в `mood_log.txt`. Това е много силно допълнение.", false, writeCorrection, false},
		{"a write again", "Записвам го отново в `mood_log.txt`. Запазих: „не се предавам“.", false, writeCorrection, false},
		{"a write that was really made", "Записах го в `mood_log.txt`.", true, "", false},
		{"a memory, not a file", "Записах го в паметта си.", false, "", false},
		{"a book, not a file", "Добавих книгата в списъка за четене.", false, "", false},
		{"no claim", "Файлът `mood_log.txt` съдържа един ред.", false, "", false},
		{"a plan to write, not a claim", "Ще го запиша във файла веднага щом ми кажеш името.", false, "", false},
		{"English", "I saved it to notes.txt for you.", false, writeCorrection, false},
		{"English removal", "I deleted the file notes.txt.", false, removalCorrection, false},
		{"empty", "", false, "", false},
		{"no file named, but the message was about one and no tool ran", "Записах го веднага. Тази малка доза оптимизъм помага.", false, writeCorrection, true},
		{"the same words to a message about no file", "Записах го веднага. Тази малка доза оптимизъм помага.", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, unbackedFileClaim(tt.answer, tt.wrote, tt.asked))
		})
	}
}

func TestTheNoteIsOnTheRequestToTheModelAndNowhereElse(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Нямам инструмент за изтриване.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "Изтрий файла mood_log.txt"},
	}

	content, _, _, err := o.generateResponse(t.Context(), history, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Нямам инструмент за изтриване.", content, "an honest answer is left alone")
	require.Len(t, mock.Requests, 1)
	sent := mock.Requests[0].Messages
	assert.Contains(t, sent[len(sent)-1].Content, "Изтрий файла mood_log.txt")
	assert.Contains(t, sent[len(sent)-1].Content, "no tool that deletes")
	assert.Equal(t, "Изтрий файла mood_log.txt", history[1].Content, "the caller's messages are not changed: the note is not stored")
}

func TestAnOrdinaryRequestGetsNoNote(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("Здравей")}}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	_, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши ми това във файл бележки.md"}}, o.turnTools(), nil)

	require.NoError(t, err)
	sent := mock.Requests[0].Messages
	assert.Equal(t, "Запиши ми това във файл бележки.md", sent[len(sent)-1].Content)
}

func TestAnAnswerThatClaimsARemovalGetsACorrectionUnderIt(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Разбрах, изтрих файла `mood_log.txt` от папката ти.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})
	var streamed strings.Builder

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "кажи нещо"}}, o.turnTools(),
		func(delta, _ string) error { streamed.WriteString(delta); return nil })

	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(content, removalCorrection), content)
	assert.True(t, strings.HasPrefix(content, "Разбрах, изтрих файла"), "the answer itself stays")
	assert.Contains(t, streamed.String(), removalCorrection, "the client sees the correction as part of the stream")
	assert.Equal(t, content, streamed.String()+"", "what was streamed is what is stored")
}

func TestAWriteClaimWithoutAWriteIsCorrected(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Записах го в `mood_log.txt`.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	// onDelta is nil for Telegram: the correction is in the returned text.
	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "добави във същия файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(content, writeCorrection), content)
}

func TestAWriteClaimAfterARealWriteIsLeftAlone(t *testing.T) {
	e, store := newFilesToolEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
		{toolCallChunk(0, "call_2", writeFileTool, `{"name":"бележки.md","content":"мляко"}`)},
		{textChunk("Записах го във файла бележки.md.")},
	}}
	o := newGroupOrchestrator(mock, e.reg)

	content, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши мляко във файл бележки.md"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Записах го във файла бележки.md.", content)
	assert.Equal(t, "мляко", e.fileText(t, store, "бележки.md"))
}

func TestAnAppendCountsAsAWrite(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "list.txt", "мляко")
	require.NoError(t, err)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
		{toolCallChunk(0, "call_2", appendFileTool, `{"name":"list.txt","content":"хляб"}`)},
		{textChunk("Добавих хляб във файла list.txt.")},
	}}
	o := newGroupOrchestrator(mock, e.reg)

	content, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Добави хляб в list.txt"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Добавих хляб във файла list.txt.", content)
}

// A write that waits for the user is not a write: "Записах" would be false.
func TestAProposalIsNotAWriteForTheGuard(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "plan.md", "стар")
	require.NoError(t, err)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", filesStubTool, `{}`)},
		{toolCallChunk(0, "call_2", writeFileTool, `{"name":"plan.md","content":"нов"}`)},
		{textChunk("Записах новия план във файла plan.md.")},
	}}
	o := newGroupOrchestrator(mock, e.reg)

	content, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши наново plan.md"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(content, writeCorrection), "the file was not written; it waits for the user: %s", content)
	assert.Equal(t, "стар", e.fileText(t, store, "plan.md"))
}

func TestWithoutTheFileGroupNothingIsChecked(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Изтрих файла notes.txt.")},
	}}
	o := &Orchestrator{llmClient: mock, tools: &fakeToolRegistry{}}

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Изтрий файла notes.txt"}}, toolDefinitions(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Изтрих файла notes.txt.", content, "the check belongs to the file tools")
	assert.Equal(t, "Изтрий файла notes.txt", mock.Requests[0].Messages[0].Content, "and so does the note")
}

func TestAFailedGenerationAddsNothing(t *testing.T) {
	mock := &llm.MockLLM{
		StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("Изтрих файла notes.txt.")}},
		StreamErrors: []error{assert.AnError},
	}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "x"}}, o.turnTools(), nil)

	require.Error(t, err)
	assert.NotContains(t, content, "Поправка")
}

// P52: a write claim with no file tool called is asked again, with the file tools open and
// a call required; what is stored is the second answer alone.
func TestAWriteClaimWithoutAToolIsAskedAgainWithTheFileTools(t *testing.T) {
	e, store := newFilesToolEnv(t)
	_, err := store.Create(e.userID, "test_file.txt", "Здравей")
	require.NoError(t, err)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Добавих в test_file.txt една мисъл.")},
		{toolCallChunk(0, "call_1", appendFileTool, `{"name":"test_file.txt","content":"Кодът е изкуство."}`)},
		{textChunk("Добавих ред в test_file.txt.")},
	}}
	o := newGroupOrchestrator(mock, e.reg)
	var streamed strings.Builder
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "допиши във същия файл нещо"}}

	content, _, executed, err := o.generateResponse(ctx, history, o.turnTools(),
		func(delta, _ string) error { streamed.WriteString(delta); return nil })

	require.NoError(t, err)
	assert.Equal(t, "Добавих ред в test_file.txt.", content, "the stored answer is the second one, without the false claim")
	assert.Contains(t, e.fileText(t, store, "test_file.txt"), "Кодът е изкуство.")
	assert.Equal(t, []string{appendFileTool}, toolCallNames(executed))
	assert.Equal(t, "Добавих в test_file.txt една мисъл."+retryNotice+"Добавих ред в test_file.txt.", streamed.String(),
		"the client saw the first answer, the line under it and the second answer")

	require.Len(t, mock.Requests, 3)
	retry := mock.Requests[1]
	assert.Equal(t, "required", retry.ToolChoice)
	assert.ElementsMatch(t, []string{listFilesTool, readFileTool, writeFileTool, appendFileTool}, toolNames(retry.Tools))
	last := retry.Messages[len(retry.Messages)-1]
	assert.Contains(t, last.Content, "no file tool was called")
	assert.Equal(t, "допиши във същия файл нещо", history[0].Content, "the note is not stored")
	assert.Nil(t, mock.Requests[2].ToolChoice, "after the call the model is free again")
	assert.NotContains(t, toolNames(mock.Requests[2].Tools), filesStubTool, "the group stays open")
}

func TestARetryThatStillWritesNothingIsCorrected(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Записах го в `mood_log.txt`.")},
		{textChunk("Записах го пак в `mood_log.txt`.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "добави във същия файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 2, "one retry, no more")
	assert.Equal(t, "Записах го пак в `mood_log.txt`.\n\n"+writeCorrection, content)
}

// After the web the retry does not offer read_file, as no round of such a turn does.
func TestTheRetryKeepsTheRuleOfFilesAndWeb(t *testing.T) {
	ctx, turn := withTurn(t.Context(), "s", false)
	turn.markExternal(sourceWebSearch)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Записах времето във файла времето.txt.")},
		{textChunk("Записах го.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	_, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "запиши го във файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 2)
	assert.NotContains(t, toolNames(mock.Requests[1].Tools), readFileTool)
	assert.Contains(t, toolNames(mock.Requests[1].Tools), writeFileTool)
}

func TestARemovalClaimIsNotRetried(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Изтрих файла notes.txt и записах нов файл.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "кажи нещо"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Len(t, mock.Requests, 1, "no tool deletes: a retry cannot make the claim true")
	assert.True(t, strings.HasSuffix(content, removalCorrection), content)
}

func TestAnHonestAnswerIsNotRetried(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk("Добре, кажи ми името на файла.")}}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	_, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "запиши нещо във файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Len(t, mock.Requests, 1)
}

func toolCallNames(calls []openai.ToolCall) []string {
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Function.Name)
	}
	return names
}

// "Записах го веднага." names no file; the message did, and nothing was called.
func TestAClaimThatNamesNoFileIsRetriedWhenTheMessageNamedOne(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{textChunk("Записах го веднага.")},
		{textChunk("Записах го.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "добави във същият файл - не се предавам"}}, o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 2, "asked again with the file tools")
	assert.Equal(t, "required", mock.Requests[1].ToolChoice)
	assert.True(t, strings.HasSuffix(content, writeCorrection), content)
}

func TestAClaimAfterAnotherToolIsNotTakenForAFile(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "create_memory", `{"content":"файлът е важен"}`)},
		{textChunk("Записах го.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})

	content, _, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "запомни, че този файл ми е важен"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Len(t, mock.Requests, 2, "no retry")
	assert.Equal(t, "Записах го.", content)
}
