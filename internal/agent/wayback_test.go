package agent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/llm"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

var copyDay = time.Date(2026, 7, 16, 8, 9, 10, 0, time.UTC)

// archivePages is the local fetch of a test with a copy in the Wayback Machine.
type archivePages struct {
	*fakePages
	copyText string
	copyErr  error
	asked    int
}

func (a *archivePages) ArchivedText(_ context.Context, rawURL string) (string, string, string, time.Time, error) {
	a.asked++
	if a.copyErr != nil {
		return "", "", "", time.Time{}, a.copyErr
	}
	return "Overnight returns (copy)", a.copyText, "https://web.archive.org/web/20260716080910id_/" + rawURL, copyDay, nil
}

func archiveWebEnv(t *testing.T, localErr error) (*DefaultToolRegistry, *fakeWeb, *archivePages, context.Context, *turnState) {
	t.Helper()
	pages := &archivePages{fakePages: &fakePages{err: localErr}, copyText: strings.Repeat(pageText, 5)}
	r, web, ctx, turn := localWebEnv(t, nil)
	r.WithLocalWebFetch(pages).WithWayback()
	return r, web, pages, ctx, turn
}

// A page that is gone (404) is read from its copy, and the paid API is not asked: it would get the
// 404 too.
func TestWebFetchReadsAGonePageFromItsCopy(t *testing.T) {
	r, web, pages, ctx, turn := archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonNotFound, Detail: "the site answered 404"})
	before := counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "wayback"))

	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/gone"})

	require.NoError(t, err)
	out, page := pageResult(t, res)
	assert.Equal(t, "wayback", out["fetched"])
	assert.Equal(t, true, out["untrusted"])
	assert.Equal(t, webContentNotice, out["notice"])
	note, _ := out["archived_copy"].(string)
	assert.Contains(t, note, "the page does not exist (404)")
	assert.Contains(t, note, "Wayback Machine (web.archive.org) kept on 2026-07-16")
	assert.Contains(t, note, "Tell the user")
	assert.Equal(t, strings.Repeat(pageText, 5), page["content"])
	assert.Contains(t, page["url"], "web.archive.org/web/20260716080910id_/https://example.org/gone")
	assert.Zero(t, web.hits.Load(), "the paid API was not asked")
	assert.Equal(t, 1, pages.asked)
	assert.True(t, turn.has(sourceWebFetch))
	assert.Equal(t, before+1, counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "wayback")))
}

// A site that refuses a program: the paid API reads the page as it is now, and the archive is not asked.
func TestWebFetchAsksThePaidAPIBeforeTheArchiveForABlockedPage(t *testing.T) {
	r, web, pages, ctx, _ := archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonBlocked})
	web.handler = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"Live","content":"The page as it is today, read by the paid API."}`)
	}
	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/blocked"})
	require.NoError(t, err)
	out, page := pageResult(t, res)
	assert.NotContains(t, out, "archived_copy")
	assert.Equal(t, "Live", page["title"])
	assert.Zero(t, pages.asked)
}

// ... and when the paid API cannot read it either, the copy is the last road.
func TestWebFetchReadsABlockedPageFromItsCopyWhenThePaidAPIFails(t *testing.T) {
	r, web, pages, ctx, _ := archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonBlocked})
	web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/blocked"})
	require.NoError(t, err)
	out, _ := pageResult(t, res)
	assert.Contains(t, out["archived_copy"], "the site refuses programs (401/403)")
	assert.Equal(t, int32(1), web.hits.Load())
	assert.Equal(t, 1, pages.asked)
}

// No copy: the model hears why the page itself was not read, as before.
func TestWebFetchWithoutACopySaysWhyThePageWasNotRead(t *testing.T) {
	r, web, pages, ctx, turn := archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonNotFound})
	pages.copyErr = &webfetch.Error{Reason: webfetch.ReasonNotArchived}
	web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
	before := counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "wayback_not_archived"))

	_, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/gone"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the page does not exist (404)")
	assert.Equal(t, 1, pages.asked, "asked once, not again after the paid API")
	assert.False(t, turn.has(sourceWebFetch))
	assert.Equal(t, before+1, counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "wayback_not_archived")))
}

// A page with no text is the same in its copy; a page that was read is not looked for; off, nothing changes.
func TestWebFetchAsksTheArchiveOnlyWhenItCanHelp(t *testing.T) {
	r, web, pages, ctx, _ := archiveWebEnv(t, errPageEmpty)
	web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	_, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/js"})
	require.Error(t, err)
	assert.Zero(t, pages.asked, "an empty page")

	r, _, pages, ctx, _ = archiveWebEnv(t, nil)
	pages.title, pages.text = "T", pageText
	_, err = r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/fine"})
	require.NoError(t, err)
	assert.Zero(t, pages.asked, "a page that was read")

	r, web, pages, ctx, _ = archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonNotFound})
	web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
	r.wayback = false
	_, err = r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/gone"})
	require.Error(t, err)
	assert.Zero(t, pages.asked, "the archive is off")
}

// save_page keeps the copy, and the model is told that it is one.
func TestSavePageTakesTheCopyOfAGonePage(t *testing.T) {
	r, web, _, ctx, _ := archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonNotFound})
	title, text, final, note, err := r.pageText(ctx, "https://example.org/gone")
	require.NoError(t, err)
	assert.Equal(t, "Overnight returns (copy)", title)
	assert.Equal(t, strings.Repeat(pageText, 5), text)
	assert.Contains(t, final, "web.archive.org", "the file names the copy as its source")
	assert.Contains(t, note, "kept on 2026-07-16")
	assert.Zero(t, web.hits.Load())

	r, web, pages, ctx, _ := archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonTimeout})
	web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	_, _, _, note, err = r.pageText(ctx, "https://example.org/slow")
	require.NoError(t, err)
	assert.Contains(t, note, "did not answer in time")
	assert.Equal(t, int32(1), web.hits.Load(), "the paid API first after a time-out")
	assert.Equal(t, 1, pages.asked)
}

// archiveFetcherFake is the fetcher of add_to_library with a Wayback Machine.
type archiveFetcherFake struct {
	*fakeFetcher
	copies map[string]*webfetch.Document
	mu     sync.Mutex // the links of one call are fetched at the same time
	asked  []string
}

func (a *archiveFetcherFake) Archived(_ context.Context, link string) (*webfetch.Document, error) {
	a.mu.Lock()
	a.asked = append(a.asked, link)
	a.mu.Unlock()
	if doc, ok := a.copies[link]; ok {
		return doc, nil
	}
	return nil, &webfetch.Error{Reason: webfetch.ReasonNotArchived}
}

func TestAddToLibraryTakesTheCopyOfALinkThatRefuses(t *testing.T) {
	text := "https://a.org/blocked.pdf https://b.org/gone.pdf https://c.org/abstract"
	ctx, turn := libraryAddCtx(t, text)
	copied := pdfDoc("blocked.pdf")
	copied.Archived = copyDay
	f := &archiveFetcherFake{
		fakeFetcher: &fakeFetcher{errs: map[string]error{
			"https://a.org/blocked.pdf": &webfetch.Error{Reason: webfetch.ReasonBlocked, Detail: "the site answered 403"},
			"https://b.org/gone.pdf":    &webfetch.Error{Reason: webfetch.ReasonNotFound, Detail: "the site answered 404"},
			"https://c.org/abstract":    &webfetch.Error{Reason: webfetch.ReasonAbstractOnly, Detail: "only an abstract"},
		}},
		copies: map[string]*webfetch.Document{"https://a.org/blocked.pdf": copied},
	}
	a := &fakeAdder{}
	r := addTool(f, a).WithWayback()
	before := counterValue(t, metrics.LibraryAdd.WithLabelValues("wayback"))

	rows := rowsOf(t, call(t, r, ctx, "https://a.org/blocked.pdf", "https://b.org/gone.pdf", "https://c.org/abstract"))

	require.Len(t, rows, 3)
	assert.Equal(t, addQueued, rows[0].Status)
	assert.Equal(t, "blocked.pdf", rows[0].File)
	assert.Contains(t, rows[0].Archived, "kept on 2026-07-16")
	assert.Contains(t, a.added["blocked.pdf"], "%PDF-1.4", "the copy is what went into the library")
	assert.Equal(t, addRefused, rows[1].Status, "no copy")
	assert.Equal(t, "not_found", rows[1].Code)
	assert.Equal(t, addRefused, rows[2].Status)
	assert.ElementsMatch(t, []string{"https://a.org/blocked.pdf", "https://b.org/gone.pdf"}, f.asked, "an abstract is not looked for")
	assert.Equal(t, before+1, counterValue(t, metrics.LibraryAdd.WithLabelValues("wayback")))

	report := turn.libraryReport()
	assert.Contains(t, report, "⚠️ Не са добавени в библиотеката (2 от 3)")
	assert.Contains(t, report, "ℹ️ Взети от архива web.archive.org")
	assert.Contains(t, report, "• https://a.org/blocked.pdf — копие от 16.07.2026 (самата страница: сайтът не пуска програми (401/403))")
}

func TestAddToLibraryWithoutTheArchiveIsAsBefore(t *testing.T) {
	ctx, turn := libraryAddCtx(t, "https://a.org/blocked.pdf")
	f := &archiveFetcherFake{fakeFetcher: &fakeFetcher{errs: map[string]error{"https://a.org/blocked.pdf": &webfetch.Error{Reason: webfetch.ReasonBlocked}}},
		copies: map[string]*webfetch.Document{"https://a.org/blocked.pdf": pdfDoc("blocked.pdf")}}
	rows := rowsOf(t, call(t, addTool(f, &fakeAdder{}), ctx, "https://a.org/blocked.pdf"))
	assert.Equal(t, addRefused, rows[0].Status)
	assert.Empty(t, f.asked)
	assert.NotContains(t, turn.libraryReport(), "архива")
}

// The local fetcher of web_fetch reads the copy through the archive of its fetcher.
func TestTheLocalFetcherReadsTheTextOfACopy(t *testing.T) {
	doc := &webfetch.Document{Name: "a.html", Kind: webfetch.KindHTML, Body: strings.NewReader("<p>x</p>"),
		Final: "https://web.archive.org/web/20260716080910id_/https://example.org/a", Archived: copyDay}
	f := &localPageFetcher{docs: &archiveDocs{doc: doc}, html: fakeExtractor{text: strings.Repeat(pageText, 5)}}
	title, text, final, at, err := f.ArchivedText(t.Context(), "https://example.org/a")
	require.NoError(t, err)
	assert.Equal(t, "a", title)
	assert.Equal(t, strings.TrimSpace(strings.Repeat(pageText, 5)), text)
	assert.Equal(t, doc.Final, final)
	assert.Equal(t, copyDay, at)

	_, _, _, _, err = (&localPageFetcher{docs: fakeDocs{doc: doc}}).ArchivedText(t.Context(), "https://example.org/a")
	assert.ErrorIs(t, err, errNoArchive)
}

type archiveDocs struct{ doc *webfetch.Document }

func (a *archiveDocs) Fetch(context.Context, string) (*webfetch.Document, error) {
	return nil, &webfetch.Error{Reason: webfetch.ReasonNotFound}
}

func (a *archiveDocs) Archived(context.Context, string) (*webfetch.Document, error) {
	return a.doc, nil
}

func TestTheArchiveReport(t *testing.T) {
	_, turn := withTurn(context.Background(), "s", false)
	assert.Empty(t, turn.archiveReport())

	turn.addArchivedCopy(archivedCopy{url: "https://example.org/a", on: copyDay, reason: string(webfetch.ReasonBlocked)})
	turn.addArchivedCopy(archivedCopy{url: "https://example.org/a", on: copyDay, reason: string(webfetch.ReasonBlocked)})
	assert.Equal(t, "ℹ️ Текстът на https://example.org/a е от архива web.archive.org — копие от 16.07.2026 (самата страница: сайтът не пуска програми (401/403)). Страницата може да се е променила оттогава.",
		turn.archiveReport(), "the same page once")

	turn.addArchivedCopy(archivedCopy{url: "https://example.org/b", on: copyDay.AddDate(-1, 0, 0), reason: string(webfetch.ReasonNotFound)})
	report := turn.archiveReport()
	assert.Contains(t, report, "ℹ️ Взети от архива web.archive.org")
	assert.Contains(t, report, "• https://example.org/a — копие от 16.07.2026")
	assert.Contains(t, report, "• https://example.org/b — копие от 16.07.2025 (самата страница: не е намерен (404))")
}

// 2026-11-26, the first live run: the model summed up the copy and did not say it was one. The
// line under the answer is the code's, whatever the model says.
func TestTheAnswerSaysThatThePageIsACopyWhateverTheModelSays(t *testing.T) {
	r, web, pages, _, _ := archiveWebEnv(t, &webfetch.Error{Reason: webfetch.ReasonBlocked})
	web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	t.Cleanup(func() { assert.Equal(t, 1, pages.asked) })
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "web_fetch", `{"url":"https://example.org/blocked"}`)},
		{textChunk("Статията разглежда нощната доходност.")},
	}}
	o := newGroupOrchestrator(mock, r)
	ctx, _ := withTurn(context.WithValue(context.Background(), api.ParsedUserIDKey, uuid.New()), "s", false)
	var streamed strings.Builder

	content, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "прочети https://example.org/blocked"}}, o.turnTools(),
		func(delta, _ string) error { streamed.WriteString(delta); return nil })

	require.NoError(t, err)
	assert.Equal(t, []string{"web_fetch"}, toolCallNames(executed))
	assert.True(t, strings.HasPrefix(content, "Статията разглежда нощната доходност."))
	assert.Contains(t, content, "ℹ️ Текстът на https://example.org/blocked е от архива web.archive.org — копие от 16.07.2026")
	assert.Equal(t, content, streamed.String(), "what was streamed is what is stored")
}

// A page read from its site gets no such line.
func TestAnAnswerFromALivePageGetsNoArchiveLine(t *testing.T) {
	r, _, pages, _, _ := archiveWebEnv(t, nil)
	pages.title, pages.text = "T", strings.Repeat(pageText, 5)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "web_fetch", `{"url":"https://example.org/fine"}`)},
		{textChunk("Прочетох я.")},
	}}
	o := newGroupOrchestrator(mock, r)
	ctx, _ := withTurn(context.WithValue(context.Background(), api.ParsedUserIDKey, uuid.New()), "s", false)
	content, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "прочети https://example.org/fine"}}, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Прочетох я.", content)
}
