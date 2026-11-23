package agent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/internal/proposals"
	"go-monolith/internal/userfiles"
	"go-monolith/internal/webfetch"
)

// savePageEnv: the file tools on app_test, the local fetch fake, the paid API fake.
func savePageEnv(t *testing.T, pages *fakePages) (*toolEnv, *userfiles.Store, context.Context, *turnState) {
	t.Helper()
	e, store := newFilesToolEnv(t)
	e.reg.WithLocalWebFetch(pages)
	previous := savePageClock
	savePageClock = func() time.Time { return time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { savePageClock = previous })
	ctx, turn := e.turn("s", false)
	return e, store, ctx, turn
}

// A long page: more than a file of the model may hold, all of it in the file.
var longPage = strings.Repeat("Overnight returns reverse intraday returns, and the article says why at length.\n", 1200)

func TestSavePageProposesTheWholePageAndTheApprovalWritesIt(t *testing.T) {
	require.Greater(t, len(longPage), userfiles.DefaultMaxFileBytes, "the page is larger than a file of the model")
	e, store, ctx, turn := savePageEnv(t, &fakePages{title: "Overnight Returns: A Puzzle", text: longPage, final: "https://example.org/paper.pdf"})

	res, err := e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "https://example.org/paper"})

	require.NoError(t, err)
	out := resultMap(t, res)
	assert.Equal(t, statusPendingApproval, out["status"])
	assert.Equal(t, "Overnight Returns A Puzzle.md", out["file_name"], "named after the title")
	assert.Contains(t, out["message"], "NOT saved yet")
	assert.Contains(t, out["message"], "you have not read it")
	assert.NotContains(t, out, "content", "the model is not given the page")
	assert.True(t, turn.has(sourceWebFetch))
	_, err = store.Stat(e.userID, "Overnight Returns A Puzzle.md")
	assert.ErrorIs(t, err, userfiles.ErrNotFound, "nothing is written before the user confirms")

	pending := e.pending(t)
	require.Len(t, pending, 1)
	p := pending[0]
	assert.Equal(t, proposals.WriteCreate, p.FileMode)
	assert.Equal(t, "https://example.org/paper.pdf", p.Source)
	assert.Contains(t, p.Summary, "Нов файл с цялата страница")
	assert.Contains(t, p.Summary, "example.org")
	assert.Contains(t, p.Content, "ще бъдат записани всичките", "the client is shown the start of a long page")
	assert.Less(t, len([]rune(p.Content)), len([]rune(longPage)))
	assert.Equal(t, []string{"web_fetch"}, p.ExternalSources)

	_, err = e.svc.Approve(context.Background(), e.userID, p.ID)
	require.NoError(t, err)
	text, _, err := store.Text(e.userID, "Overnight Returns A Puzzle.md")
	require.NoError(t, err)
	assert.Equal(t, "# Overnight Returns: A Puzzle\n\nИзточник: https://example.org/paper.pdf\nСвалено: 2026-11-23\n\n"+strings.TrimSpace(longPage)+"\n", text,
		"the file is the page, word for word")
}

func TestSavePageTakesTheGivenNameAndAFreeOne(t *testing.T) {
	e, store, ctx, _ := savePageEnv(t, &fakePages{title: "T", text: pageText + pageText + pageText})
	_, err := store.Create(e.userID, "notes.md", "мои бележки")
	require.NoError(t, err)

	res, err := e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "https://example.org/a", "name": "notes"})

	require.NoError(t, err)
	assert.Equal(t, "notes (2).md", resultMap(t, res)["file_name"], "the extension added, the taken name kept")
	text, _, _ := store.Text(e.userID, "notes.md")
	assert.Equal(t, "мои бележки", text, "the file that was there is not touched")
}

func TestSavePageRefusesABadName(t *testing.T) {
	e, _, ctx, _ := savePageEnv(t, &fakePages{title: "T", text: pageText})
	_, err := e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "https://example.org/a", "name": "../evil.md"})
	var argErr *toolArgumentError
	require.ErrorAs(t, err, &argErr)
	assert.Empty(t, e.pending(t))
}

// The site refuses a program: the paid API gives the page.
func TestSavePageFallsBackToThePaidAPI(t *testing.T) {
	e, _, ctx, _ := savePageEnv(t, &fakePages{err: &webfetch.Error{Reason: webfetch.ReasonBlocked}})
	e.web.handler = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"Стоицизъм","content":"`+strings.Repeat("Стоицизмът е философска школа. ", 20)+`"}`)
	}

	res, err := e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "https://bg.wikipedia.org/wiki/x"})

	require.NoError(t, err)
	assert.Equal(t, "Стоицизъм.md", resultMap(t, res)["file_name"])
	assert.Equal(t, "/fetch", e.web.lastReq.path)
	pending := e.pending(t)
	require.Len(t, pending, 1)
	assert.Contains(t, pending[0].Content, "Стоицизмът е философска школа.")
	assert.Equal(t, "https://bg.wikipedia.org/wiki/x", pending[0].Source)
}

func TestSavePageSaysWhyWhenThePageCannotBeRead(t *testing.T) {
	e, _, ctx, turn := savePageEnv(t, &fakePages{err: &webfetch.Error{Reason: webfetch.ReasonBlocked}})
	e.web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }

	_, err := e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "https://stackoverflow.com/q/1"})

	var argErr *toolArgumentError
	require.ErrorAs(t, err, &argErr)
	assert.Equal(t, savePageTool, argErr.tool)
	assert.Contains(t, err.Error(), "the site refuses programs")
	assert.Empty(t, e.pending(t))
	assert.False(t, turn.has(sourceWebFetch))
}

// A page the paid API gives almost empty is not saved.
func TestSavePageDoesNotSaveAnEmptyPage(t *testing.T) {
	e, _, ctx, _ := savePageEnv(t, &fakePages{err: errPageEmpty})
	e.web.handler = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"X","content":"Enable JavaScript"}`)
	}

	_, err := e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "https://x.com/"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no text that could be read")
	assert.Empty(t, e.pending(t))
}

// The user's files and the web are not used in one turn (P44).
func TestSavePageIsRefusedAfterAFileWasRead(t *testing.T) {
	pages := &fakePages{title: "T", text: pageText}
	e, store, ctx, _ := savePageEnv(t, pages)
	_, err := store.Create(e.userID, "secret.md", "тайна")
	require.NoError(t, err)
	_, err = e.reg.ExecuteTool(ctx, readFileTool, map[string]interface{}{"name": "secret.md"})
	require.NoError(t, err)

	_, err = e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "https://example.org/a"})

	require.Error(t, err)
	assert.Zero(t, pages.calls, "no page was fetched")
	assert.Empty(t, e.pending(t))
}

func TestSavePageChecksTheAddressAndAsksForIt(t *testing.T) {
	pages := &fakePages{title: "T", text: pageText}
	e, _, ctx, _ := savePageEnv(t, pages)

	_, err := e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{"url": "http://127.0.0.1/admin"})
	require.Error(t, err)
	_, err = e.reg.ExecuteTool(ctx, savePageTool, map[string]interface{}{})
	require.Error(t, err)
	assert.Zero(t, pages.calls)
}

func TestPageFileNames(t *testing.T) {
	for _, c := range []struct{ given, title, want string }{
		{"", "Betting Against Beta | NBER", "Betting Against Beta NBER.md"},
		{"", "Стоицизъм — Уикипедия", "Стоицизъм Уикипедия.md"},
		{"", "", "страница.md"},
		{"", "???", "страница.md"},
		{"", "--- x", "x.md"},
		{"article.txt", "T", "article.txt"},
		{" статия ", "T", "статия.md"},
		{"", strings.Repeat("дълго ", 40), strings.TrimSpace(string([]rune(strings.Repeat("дълго ", 40))[:userfiles.MaxNameRunes-len(".md")-len(" (99)")])) + ".md"},
	} {
		got := pageFileName(c.given, c.title)
		assert.Equal(t, c.want, got, "%q %q", c.given, c.title)
		assert.NoError(t, userfiles.ValidateName(got), got)
	}
}

func TestTheToolIsInTheGroupOfTheFiles(t *testing.T) {
	assert.Contains(t, toolNames(fileToolDefinitions()), savePageTool)
	def := savePageDefinition().Function
	assert.Contains(t, def.Description, "WHOLE text")
	assert.Contains(t, def.Description, writeFileTool, "the own summary goes to write_file")
	assert.Equal(t, savePageTool, toolLabel(savePageTool))
}

func TestAsksToSavePage(t *testing.T) {
	yes := []string{
		"Запиши цялата статия във файл: https://arxiv.org/abs/1706.03762",
		"Свали тази страница във файл: https://go.dev/doc/effective_go",
		"Запази текста на тази статия в attention.md: https://arxiv.org/abs/1706.03762",
		"Save this whole page to a file: https://pkg.go.dev/net/http",
		"Искам да имам статията като файл, целия текст: https://www.nature.com/articles/x",
		"https://example.org/a.pdf запази го като файл",
	}
	no := []string{
		"Прочети https://arxiv.org/abs/1706.03762 и запиши кратко резюме в resume.md",
		"Направи ми бележки по тази страница и ги запиши във файл: https://go.dev/doc/effective_go",
		"Прочети https://arxiv.org/abs/1706.03762 и ми кажи за какво е",
		"Запиши цялата статия във файл",
		"Добави https://example.org/a.pdf в библиотеката",
		"Свали https://example.org/a.pdf в библиотеката ми",
		"Преведи https://example.org/a и го запиши във файл",
		"Запиши във файл https://example.org/a",
		"Save the key points of https://example.org/a to a file",
	}
	for _, m := range yes {
		assert.True(t, asksToSavePage(m), m)
	}
	for _, m := range no[:len(no)-2] {
		assert.False(t, asksToSavePage(m), m)
	}
	assert.True(t, asksToSavePage(no[len(no)-2]), "a link and a file and a word for saving: the page")
	assert.False(t, asksToSavePage(no[len(no)-1]), no[len(no)-1])
}

// Such a message opens with save_page alone, required, though the group of the files is locked.
func TestTheWholePageAsAFileIsForced(t *testing.T) {
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", savePageTool, `{"url":"https://arxiv.org/abs/1706.03762"}`)},
		{textChunk("Страницата е готова за запис.")},
	}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{result: map[string]string{"status": statusPendingApproval}})

	_, executed, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши цялата статия във файл: https://arxiv.org/abs/1706.03762"}}, o.turnTools(), nil)

	require.NoError(t, err)
	require.Len(t, mock.Requests, 2)
	assert.Equal(t, []string{savePageTool}, toolNames(mock.Requests[0].Tools))
	assert.Equal(t, "required", mock.Requests[0].ToolChoice)
	assert.Equal(t, []string{savePageTool}, toolCallNames(executed))
	assert.Contains(t, toolNames(mock.Requests[1].Tools), filesStubTool, "the rest of the turn as before")
}

// A summary into a file is the model's own text: nothing is forced.
func TestASummaryOfAPageIsNotForced(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Добре.")}}
	o := newGroupOrchestrator(mock, &fakeToolRegistry{})
	_, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Прочети https://arxiv.org/abs/1706.03762 и запиши кратко резюме в resume.md"}}, o.turnTools(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, mock.Requests)
	assert.Empty(t, mock.Requests[0].ToolChoice)
}

// Without the files there is no save_page to force.
func TestNoSavePageWithoutTheFiles(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Добре.")}}
	o := &Orchestrator{llmClient: mock, tools: &fakeToolRegistry{}}
	_, _, err := o.generateResponse(t.Context(),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "Запиши цялата статия във файл: https://arxiv.org/abs/1706.03762"}}, o.turnTools(), nil)
	require.NoError(t, err)
	assert.NotContains(t, toolNames(mock.Requests[0].Tools), savePageTool)
	assert.Empty(t, mock.Requests[0].ToolChoice)
}
