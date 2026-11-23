package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/ingest"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// fakePages stands in for the local fetch of web_fetch.
type fakePages struct {
	title, text, final string
	err                error
	calls              int
}

func (f *fakePages) FetchText(_ context.Context, rawURL string) (string, string, string, error) {
	f.calls++
	if f.err != nil {
		return "", "", "", f.err
	}
	final := f.final
	if final == "" {
		final = rawURL
	}
	return f.title, f.text, final, nil
}

// localWebEnv is a registry with the local fetch and a fake paid API.
func localWebEnv(t *testing.T, pages *fakePages) (*DefaultToolRegistry, *fakeWeb, context.Context, *turnState) {
	t.Helper()
	web := newFakeWeb(t)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	r := &DefaultToolRegistry{httpClient: web.server.Client(), webBaseURL: web.server.URL}
	r.WithLocalWebFetch(pages)
	ctx, turn := withTurn(context.Background(), "s", false)
	return r, web, ctx, turn
}

func pageResult(t *testing.T, res interface{}) (map[string]interface{}, map[string]interface{}) {
	t.Helper()
	out, ok := res.(map[string]interface{})
	require.True(t, ok, "a map: %#v", res)
	inner, ok := out["result"].(map[string]interface{})
	require.True(t, ok, "the page is under result: %#v", out)
	return out, inner
}

const pageText = "Overnight returns reverse intraday returns, and the article says why at length. "

func TestWebFetchReadsThePageHereFirst(t *testing.T) {
	pages := &fakePages{title: "Overnight returns", text: strings.Repeat(pageText, 5), final: "https://example.org/paper.pdf"}
	r, web, ctx, turn := localWebEnv(t, pages)
	before := counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "local"))

	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/paper"})

	require.NoError(t, err)
	out, page := pageResult(t, res)
	assert.Equal(t, "local", out["fetched"])
	assert.Equal(t, true, out["untrusted"], "the text of a page is never trusted")
	assert.Equal(t, webContentNotice, out["notice"])
	assert.Equal(t, "Overnight returns", page["title"])
	assert.Equal(t, strings.Repeat(pageText, 5), page["content"])
	assert.Equal(t, "https://example.org/paper.pdf", page["url"], "where the page really came from")
	assert.Zero(t, web.hits.Load(), "the paid API was not asked")
	assert.True(t, turn.has(sourceWebFetch), "the turn read a page from the web")
	assert.Equal(t, before+1, counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "local")))
}

func TestWebFetchDoesNotRepeatTheAddressItWasGiven(t *testing.T) {
	r, _, ctx, _ := localWebEnv(t, &fakePages{title: "T", text: pageText})
	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/a"})
	require.NoError(t, err)
	_, page := pageResult(t, res)
	assert.NotContains(t, page, "url")
}

// The site refuses a program: the paid API reads the page, as it did before the local fetch.
func TestWebFetchFallsBackToThePaidAPIWhenThePageGivesNothing(t *testing.T) {
	r, web, ctx, turn := localWebEnv(t, &fakePages{err: &webfetch.Error{Reason: webfetch.ReasonBlocked, Detail: "the site answered 403"}})
	web.handler = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"Стоицизъм","content":"Стоицизмът е философска школа, основана от Зенон."}`)
	}
	before := counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "local_blocked"))

	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://bg.wikipedia.org/wiki/x"})

	require.NoError(t, err)
	out, page := pageResult(t, res)
	assert.NotContains(t, out, "fetched", "the paid result as the API gives it")
	assert.Equal(t, "Стоицизъм", page["title"])
	assert.Equal(t, int32(1), web.hits.Load())
	assert.Equal(t, "/fetch", web.lastReq.path)
	assert.True(t, turn.has(sourceWebFetch))
	assert.Equal(t, before+1, counterValue(t, metrics.WebToolRequests.WithLabelValues("web_fetch", "local_blocked")))
}

// Neither could read it: the model hears why from the local fetch, and not to describe the page.
func TestWebFetchSaysWhyWhenNeitherCouldReadThePage(t *testing.T) {
	r, web, ctx, turn := localWebEnv(t, &fakePages{err: &webfetch.Error{Reason: webfetch.ReasonBlocked}})
	web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }

	_, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://stackoverflow.com/q/1"})

	var argErr *toolArgumentError
	require.ErrorAs(t, err, &argErr)
	assert.Contains(t, err.Error(), "the site refuses programs")
	assert.Contains(t, err.Error(), "do not describe the page as if you had read it")
	assert.False(t, turn.has(sourceWebFetch), "nothing was read")
}

func TestWebFetchReasonsOfTheLocalFetch(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{errPageEmpty, "no text that could be read"},
		{context.DeadlineExceeded, "did not answer in time"},
		{&webfetch.Error{Reason: webfetch.ReasonNotFound}, "does not exist"},
		{errors.New("something else"), "could not be read"},
	} {
		r, web, ctx, _ := localWebEnv(t, &fakePages{err: c.err})
		web.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
		_, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), c.want, c.err.Error())
	}
}

// Without a key for the paid API the local fetch still works.
func TestWebFetchWorksHereWithoutThePaidAPI(t *testing.T) {
	r, web, ctx, _ := localWebEnv(t, &fakePages{title: "T", text: pageText})
	t.Setenv("DEEPSEEK_API_KEY", "")

	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/a"})

	require.NoError(t, err)
	_, page := pageResult(t, res)
	assert.Equal(t, pageText, page["content"])
	assert.Zero(t, web.hits.Load())
}

// The checks of the address come before any fetch, local or paid.
func TestWebFetchRefusesALocalAddressBeforeFetching(t *testing.T) {
	pages := &fakePages{title: "T", text: pageText}
	r, web, ctx, _ := localWebEnv(t, pages)

	_, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "http://127.0.0.1:8080/metrics"})

	require.Error(t, err)
	assert.Zero(t, pages.calls)
	assert.Zero(t, web.hits.Load())
}

// The real local fetcher on a server of the test: the text of a page, its title, the reasons.
func TestTheLocalFetcherTakesTheTextOfAPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=utf-8><title>Overnight returns</title></head><body><script>var x = 1;</script><p>"+
			strings.Repeat(pageText, 4)+"</p></body></html>")
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<!doctype html><html><head><title>App</title></head><body><div id=root></div><noscript>Enable JavaScript</noscript></body></html>")
	})
	mux.HandleFunc("/blocked", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	var agent string
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, "<html><body><p>"+strings.Repeat(pageText, 4)+"</p></body></html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := &localPageFetcher{
		docs: webfetch.New(webfetch.Options{MinPageRunes: 1, UserAgent: webFetchUserAgent, AllowPrivate: true, Timeout: 5 * time.Second}),
		html: ingest.NewHTMLExtractor(),
		pdf:  ingest.NewPDFExtractor().WithoutOCR(),
	}

	title, text, final, err := f.FetchText(t.Context(), srv.URL+"/article")
	require.NoError(t, err)
	assert.Equal(t, "Overnight returns", title)
	assert.Contains(t, text, "Overnight returns reverse intraday returns")
	assert.NotContains(t, text, "var x", "the scripts are not text")
	assert.Equal(t, srv.URL+"/article", final)

	_, _, _, err = f.FetchText(t.Context(), srv.URL+"/empty")
	assert.ErrorIs(t, err, errPageEmpty, "a page made by JavaScript has nothing to read")

	_, _, _, err = f.FetchText(t.Context(), srv.URL+"/blocked")
	var fe *webfetch.Error
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, webfetch.ReasonBlocked, fe.Reason)

	_, _, _, err = f.FetchText(t.Context(), srv.URL+"/agent")
	require.NoError(t, err)
	assert.Equal(t, webFetchUserAgent, agent, "the fetch says what it is")
}

// The fetcher keeps the whole text (save_page writes all of it); what web_fetch gives the model
// is cut, and the budget of the turn cuts it again.
func TestTheLocalFetcherKeepsTheWholeTextAndWebFetchCutsIt(t *testing.T) {
	long := strings.Repeat("дума ", webFetchMaxRunes/4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html><body><p>"+long+"</p></body></html>")
	}))
	t.Cleanup(srv.Close)
	f := &localPageFetcher{
		docs: webfetch.New(webfetch.Options{MinPageRunes: 1, AllowPrivate: true}),
		html: ingest.NewHTMLExtractor(),
		pdf:  ingest.NewPDFExtractor().WithoutOCR(),
	}
	_, text, _, err := f.FetchText(t.Context(), srv.URL)
	require.NoError(t, err)
	assert.Greater(t, len([]rune(text)), webFetchMaxRunes, "all of the page")

	r, _, ctx, _ := localWebEnv(t, &fakePages{title: "T", text: text})
	res, err := r.executeWebFetch(ctx, map[string]interface{}{"url": "https://example.org/long"})
	require.NoError(t, err)
	_, page := pageResult(t, res)
	assert.Equal(t, webFetchMaxRunes, len([]rune(page["content"].(string))), "the model gets the start")
}

// fakeDocs gives one document; fakeExtractor gives one text, or one error.
type fakeDocs struct{ doc *webfetch.Document }

func (f fakeDocs) Fetch(context.Context, string) (*webfetch.Document, error) { return f.doc, nil }

type fakeExtractor struct {
	text string
	err  error
}

func (fakeExtractor) Name() string                                         { return "fake" }
func (fakeExtractor) Supports(string, string) bool                         { return true }
func (f fakeExtractor) Extract(context.Context, io.Reader) (string, error) { return f.text, f.err }

// A PDF goes to the extractor of PDFs, a page to the one of HTML; a PDF without a page has no
// title but its name.
func TestTheLocalFetcherReadsAPDFAsAPDF(t *testing.T) {
	f := &localPageFetcher{
		docs: fakeDocs{&webfetch.Document{Name: "w16601.pdf", Kind: webfetch.KindPDF, Body: strings.NewReader("%PDF")}},
		html: fakeExtractor{text: "the text of a page " + strings.Repeat(pageText, 3)},
		pdf:  fakeExtractor{text: "the text of a PDF " + strings.Repeat(pageText, 3)},
	}
	title, text, _, err := f.FetchText(t.Context(), "https://example.org/w16601.pdf")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(text, "the text of a PDF"), text)
	assert.Equal(t, "w16601", title)

	f.docs = fakeDocs{&webfetch.Document{Name: "page.html", Title: "A page", Kind: webfetch.KindHTML, Body: strings.NewReader("<p>")}}
	title, text, _, err = f.FetchText(t.Context(), "https://example.org/page")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(text, "the text of a page"), text)
	assert.Equal(t, "A page", title)
}

// A PDF that passes the limit while it is read says so; a PDF that is only scans has no text.
func TestTheLocalFetcherSaysWhyAPDFGaveNothing(t *testing.T) {
	pdf := &webfetch.Document{Name: "big.pdf", Kind: webfetch.KindPDF, Body: strings.NewReader("%PDF")}
	f := &localPageFetcher{docs: fakeDocs{pdf}, pdf: fakeExtractor{err: fmt.Errorf("copy: %w", webfetch.ErrTooLarge)}}
	_, _, _, err := f.FetchText(t.Context(), "https://example.org/big.pdf")
	var fe *webfetch.Error
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, webfetch.ReasonTooLarge, fe.Reason)

	f.pdf = fakeExtractor{err: ingest.ErrNoText}
	_, _, _, err = f.FetchText(t.Context(), "https://example.org/scan.pdf")
	assert.ErrorIs(t, err, errPageEmpty)
}
