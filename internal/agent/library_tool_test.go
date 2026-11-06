package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
)

func TestAsksAboutLibrary(t *testing.T) {
	asks := []string{
		// the real chat of 2026-11-05, where the model answered "Не разполагам" without a look
		"а имаш ли достъп до книгата принципи на психологията",
		"това е странно ,би трябвало да разполагаш с два тома на Принципи на психологията",
		"имаш ли книгата Принципи на психологията",
		"Има ли в библиотеката ти книга за готвене?",
		"Имаш ли файла с рецептите?",
		"какви книги имаш",
		"Колко книги имаш по психология?",
		"Кои книги по програмиране си прочела? Имаш ли Rust Atomics and Locks",
		"Do you have the book Principles of Psychology?",
		"Which books do you have on cooking?",
		"имате ли документ за лични финанси",
	}
	for _, m := range asks {
		assert.True(t, asksAboutLibrary(m), m)
	}

	doesNotAsk := []string{
		"",
		"хареса ми отговора",
		"Какво казва Марк Аврелий за смъртта?",
		"Чета една книга за стоиците и много ми харесва", // talks about a book, asks nothing
		"имаш ли време за мен",                           // asks, but not about a book
		"Колко атома има във водата?",                    // "том" inside a word
		"Имаш ли томати за салатата?",                    // "том" at the start of a word
	}
	for _, m := range doesNotAsk {
		assert.False(t, asksAboutLibrary(m), m)
	}

	// A long message is a story or a task, whatever words it has.
	long := "имаш ли книгата " + strings.Repeat("и още много думи ", 20)
	assert.False(t, asksAboutLibrary(long))
}

// With the real chat's history in front of it the model answers "Не разполагам" without
// a look. A question about what the library holds therefore opens with only
// list_documents on offer and a call required; every other question - and the round
// after the list - keeps all the tools and the model's own choice.
func TestGenerateResponseRequiresTheLibraryListForAQuestionAboutBooks(t *testing.T) {
	ask := func(question string) (*llm.MockLLM, *fakeToolRegistry) {
		mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
			{toolCallChunk(0, "call_0", listDocumentsTool, `{}`)},
			{textChunk("Да, имам ги.")},
		}}
		reg := &fakeToolRegistry{result: map[string]interface{}{"status": "success"}}
		o := newGenerateOrchestrator(mock, reg)
		_, _, _, err := o.generateResponse(context.Background(), []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "system"},
			{Role: openai.ChatMessageRoleUser, Content: question},
		}, toolDefinitions(), nil)
		require.NoError(t, err)
		return mock, reg
	}

	mock, reg := ask("а имаш ли достъп до книгата принципи на психологията")
	require.Len(t, mock.Requests, 2)
	first, second := mock.Requests[0], mock.Requests[1]
	require.Len(t, first.Tools, 1, "the first round offers one tool")
	assert.Equal(t, listDocumentsTool, first.Tools[0].Function.Name)
	assert.Equal(t, "required", first.ToolChoice)
	assert.Greater(t, len(second.Tools), 1, "the answer round has all the tools")
	assert.Nil(t, second.ToolChoice, "and the model's own choice")
	assert.Equal(t, []string{listDocumentsTool}, reg.executed)

	// The same question without a book in it is not touched.
	mock, _ = ask("а имаш ли достъп до интернет")
	require.Len(t, mock.Requests, 2)
	assert.Greater(t, len(mock.Requests[0].Tools), 1)
	assert.Nil(t, mock.Requests[0].ToolChoice)
}

// Without the tool among those on offer nothing is forced: a request must not
// ask for a tool the server has not been given.
func TestGenerateResponseDoesNotRequireAToolThatIsNotOffered(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Да.")}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{})

	_, _, _, err := o.generateResponse(context.Background(), []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "имаш ли книгата Принципи на психологията"},
	}, nil, nil)
	require.NoError(t, err)

	require.Len(t, mock.Requests, 1)
	assert.Empty(t, mock.Requests[0].Tools)
	assert.Nil(t, mock.Requests[0].ToolChoice)
}

func TestListDocumentsNamesTheFinishedFilesOnly(t *testing.T) {
	e := newToolEnv(t)
	ctx, turn := e.turn("session", false)

	add := func(name, status, category string) uuid.UUID {
		f, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
			UserID: e.userID, FileName: name, FileHash: "hash-" + uuid.NewString(),
			MimeType: "application/pdf", FileSize: 1, Status: status, LocalPath: "storage/none",
		})
		require.NoError(t, err)
		if category != "" {
			require.NoError(t, e.db.Queries.UpdateIngestedFileCategory(ctx, db.UpdateIngestedFileCategoryParams{ID: f.ID, Category: category, UserID: e.userID}))
		}
		return f.ID
	}
	add("the principlesof Psychology.epub", "completed", "psychology")
	add("Граф Монте Кристо .epub", "completed", "literature")
	add("a-book-still-being-read.pdf", "processing", "")
	deleted := add("deleted.pdf", "completed", "")
	require.NoError(t, e.db.Queries.SoftDeleteIngestedFile(ctx, db.SoftDeleteIngestedFileParams{ID: deleted, UserID: e.userID}))

	res, err := e.reg.ExecuteTool(ctx, listDocumentsTool, map[string]interface{}{})
	require.NoError(t, err)

	got := res.(map[string]interface{})
	assert.Equal(t, 2, got["count"])
	assert.Equal(t, []map[string]interface{}{
		{"name": "the principlesof Psychology.epub", "category": "psychology"},
		{"name": "Граф Монте Кристо .epub", "category": "literature"},
	}, got["files"], "by name (Latin before Cyrillic), finished and not deleted")
	assert.NotContains(t, got, "truncated")
	assert.Contains(t, turn.external, sourceListDocuments, "file names come from uploads, so memory writes of this turn wait for the user")
}

func TestListDocumentsOfAnEmptyLibrary(t *testing.T) {
	e := newToolEnv(t)
	ctx, turn := e.turn("session", false)

	res, err := e.reg.ExecuteTool(ctx, listDocumentsTool, map[string]interface{}{})
	require.NoError(t, err)

	got := res.(map[string]interface{})
	assert.Equal(t, 0, got["count"])
	assert.Empty(t, got["files"])
	assert.Empty(t, turn.external, "nothing was read")
}
