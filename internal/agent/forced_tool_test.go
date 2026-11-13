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

func TestAsksForKnowledge(t *testing.T) {
	asks := []string{
		// questions the books answer (measured on 2026-11-06: the model searched for none
		// of the first four with the chat's history)
		"Какво казва Марк Аврелий за смъртта?",
		"Как работи mutex в Rust?",
		"Какво е диверсификация на инвестиционния портфейл?",
		"Разкажи ми за подвизите на Херакъл",
		"Как се приготвя баница?",
		"Какво е теорията на относителността?",
		"Какво представляват черните дупки?",
		"Какво е Едиповият комплекс според Фройд?",
		"Как се синхронизират горутини с канали в Go?",
		"Какво е RAII в C++?",
		"Как се формират навиците според Уилям Джеймс?",
		"Какво е Дао според Лао Дзъ?",
		"Какви са симптомите на недостиг на витамин B12?",
		"Какво е рационалност според Стивън Пинкър?",
		"какво би казала според знанията по психология на човек в депресивно състояние",
		"а какво е стоицизъм според Сенека",
		"Обясни ми какво е хеш таблица",
		"What is the stream of consciousness according to William James?",
		"How do I read assembly code when reverse engineering a binary?",
		"Explain what a mutex is",
		// the books do not cover these; the search finds nothing close, at the cost of a round
		"Коя е столицата на Франция?",
		"Кой спечели световното първенство по футбол през 2018?",
	}
	for _, m := range asks {
		assert.True(t, asksForKnowledge(m), m)
	}

	doesNotAsk := []string{
		"",
		"здравей",
		"здравей, как си днес?", // opens with a greeting
		"как си",
		"как си с познанията по психология", // about the assistant
		"благодаря ти много",
		"добре, разбирам",
		"хаха, много смешно",
		"уморен съм днес",
		"какво правиш?",
		"какво?",
		"а какво се случва с теб",
		"помниш ли ме?",
		"кажи ми нещо интересно",
		"Какво ще е времето утре в София?",
		"Колко е часът?",
		"обичам те",
		"не съм съгласен с теб",
		"може ли да ми помогнеш с нещо?",
		"как мислиш, дали да си взема куче?",
		"тази сутрин изпих кафе и тръгнах към работа",
		"Прочетох статия, която обяснява защо небето е синьо", // a question word inside a statement
		"❤️🙂 хареса ми отговора",
		"не ....интересува ме как ти я използваш в разговор",
		"днес ми е добър ден, на работа всичко беше спокойно",
		"What do you think about me?",
		"How are you today?",
	}
	for _, m := range doesNotAsk {
		assert.False(t, asksForKnowledge(m), m)
	}

	// A story or a task, however it opens.
	assert.False(t, asksForKnowledge("Как"+strings.Repeat(" дума", 120)))
}

func TestAsksForPassage(t *testing.T) {
	asks := []string{
		// the request that got an invented quote on 2026-11-10, typo and all
		`даи ми някой кратък абзац от "Даото на Уорън Бъфет"`,
		// the requests of the measurement on 2026-11-13
		"цитирай Марк Аврелий за смъртта",
		"Дай ми откъс от Граф Монте Кристо",
		"Покажи ми цитат от Сенека за гнева",
		"Прочети ми абзац от Питър Пан",
		"Намери ми пасаж от Хокинг за черните дупки",
		"Цитирай Ницше от Ecce Homo",
		"Дай ми кратък откъс от Смелостта да не те харесват",
		"Преразкажи първата глава на Винету",
		"Дайте ми цитат от Лао Дзъ за водата",
		"Give me a paragraph from The Count of Monte Cristo",
		"Quote Marcus Aurelius on death",
		"Show me a passage from Hawking's A Brief History of Time",
		"Find an excerpt from William James about habit",
		"Read me a short passage from Seneca's letters",
		"Give me a quote from Ray Dalio's Principles",
		// a filler in front, a request for a page
		"моля, прочети ми страница от книгата за Дао",
		"а дай ми още един цитат",
	}
	for _, m := range asks {
		assert.True(t, asksForPassage(m), m)
	}

	doesNotAsk := []string{
		"",
		"цитирай", // nothing to quote
		// the requests for something else of the measurement on 2026-11-13
		"дай ми съвет",
		"покажи ми как се прави кафе",
		"кажи ми как си",
		"дай ми идея за вечеря довечера",
		"напиши ми стихотворение за есента",
		"покажи ми какво можеш",
		"give me a minute, I'll be right back",
		"show me some love",
		"дай ми кураж за утрешния ден",
		"find me a good name for my cat",
		"прочети ми мислите",
		// a passage word, but not a request
		"тази книга ми хареса много",
		"Вчера прочетох една глава преди сън",
		// a word that only begins like a passage word
		"дай ми главния ключ",
	}
	for _, m := range doesNotAsk {
		assert.False(t, asksForPassage(m), m)
	}

	// A story or a task, however it opens.
	assert.False(t, asksForPassage("Цитирай"+strings.Repeat(" дума", 120)))
}

func TestForcedFirstToolSearchesForARequestForAPassage(t *testing.T) {
	all := toolDefinitions()

	assert.Equal(t, searchDocumentsTool, forcedFirstTool(all, `даи ми някой кратък абзац от "Даото на Уорън Бъфет"`))
	assert.Equal(t, searchDocumentsTool, forcedFirstTool(all, "Quote Marcus Aurelius on death"))
	assert.Equal(t, "", forcedFirstTool(all, "дай ми съвет"))
	// A request that names the library is answered by the list first.
	assert.Equal(t, listDocumentsTool, forcedFirstTool(all, "Покажи откъс от книгата на Фром Да имаш или да бъдеш"))
}

func TestForcedFirstToolPrefersTheLibraryList(t *testing.T) {
	all := toolDefinitions()

	assert.Equal(t, listDocumentsTool, forcedFirstTool(all, "а имаш ли достъп до книгата принципи на психологията"))
	assert.Equal(t, listDocumentsTool, forcedFirstTool(all, "Какви книги имаш по психология?"), "a question to the books too, but the list answers it")
	assert.Equal(t, searchDocumentsTool, forcedFirstTool(all, "Какво казва Марк Аврелий за смъртта?"))
	assert.Equal(t, "", forcedFirstTool(all, "хареса ми отговора"))

	// A tool that is not offered cannot be required.
	assert.Equal(t, "", forcedFirstTool(nil, "Какво казва Марк Аврелий за смъртта?"))
	assert.Equal(t, "", forcedFirstTool(onlyTool(all, "web_search"), "имаш ли книгата Граф Монте Кристо"))
}

func TestGenerateResponseRequiresASearchForAQuestionToTheBooks(t *testing.T) {
	ask := func(question string) *llm.MockLLM {
		mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
			{toolCallChunk(0, "call_0", searchDocumentsTool, `{"query":"Марк Аврелий смърт"}`)},
			{textChunk("Според книгата...")},
		}}
		o := newGenerateOrchestrator(mock, &fakeToolRegistry{result: map[string]interface{}{"status": "success"}})
		_, _, _, err := o.generateResponse(context.Background(), []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "system"},
			{Role: openai.ChatMessageRoleUser, Content: question},
		}, toolDefinitions(), nil)
		require.NoError(t, err)
		return mock
	}

	mock := ask("Какво казва Марк Аврелий за смъртта?")
	require.Len(t, mock.Requests, 2)
	first, second := mock.Requests[0], mock.Requests[1]
	require.Len(t, first.Tools, 1, "the first round offers the search alone")
	assert.Equal(t, searchDocumentsTool, first.Tools[0].Function.Name)
	assert.Equal(t, "required", first.ToolChoice)
	assert.Greater(t, len(second.Tools), 1, "the answer round has every tool, web_search among them")
	assert.Nil(t, second.ToolChoice)

	// Small talk is left to the model: no tool is required.
	mock = &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Радвам се.")}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{})
	_, _, _, err := o.generateResponse(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "❤️🙂 хареса ми отговора"},
	}, toolDefinitions(), nil)
	require.NoError(t, err)
	require.Len(t, mock.Requests, 1)
	assert.Greater(t, len(mock.Requests[0].Tools), 1)
	assert.Nil(t, mock.Requests[0].ToolChoice)
}
