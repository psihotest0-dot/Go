package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"go-monolith/internal/ingest"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// web_fetch from this machine (plan step 3.1). The tool of the chat read pages through the paid
// API: every page cost a call, DeepSeek saw which pages Lili read, and it could not read a PDF -
// the articles the owner sent on 2026-11-21 were PDFs. With WEB_FETCH_LOCAL_ENABLED the page is
// fetched here, by the fetcher of add_to_library (internal/webfetch: the address of every
// connection and redirect is checked, the sizes are bounded, the PDF behind the page of an
// article is taken), and its text is taken out by the extractors of the ingest (HTML, and a PDF
// without OCR: a scan is not recognised during a conversation). When that gives nothing - the
// site refuses a program, the page is empty without JavaScript, a scan - the paid fetch is tried,
// as before.
//
// The result has the shape of the paid one ({"result": {"title", "content"}} in the wrapper of
// callWebTool), so the record of the tools, the budgets and the checks of what was read see no
// difference; "fetched": "local" says who fetched it.

const (
	// webFetchMaxBytes is the largest document read for a conversation (a PDF; a page is cut
	// at the limit of the fetcher, 8 MB).
	webFetchMaxBytes int64 = 20 << 20
	// webFetchTimeout bounds the whole local fetch: the request, the body and the text.
	webFetchTimeout = 30 * time.Second
	// webFetchMaxRunes bounds the text kept of a document; the budget of the tool results cuts
	// it again, to what the turn has room for.
	webFetchMaxRunes = 200_000
	// webFetchMinRunes: a page with less text than this is taken as empty (a page made by
	// JavaScript, a refusal page), and the paid fetch is tried.
	webFetchMinRunes = 200
	// webFetchUserAgent is how the local fetch names itself.
	webFetchUserAgent = "go-monolith-assistant/1.0 (a person's own assistant, reading a page for them)"
)

// pageFetcher takes the text of a page or a PDF from this machine (localPageFetcher).
type pageFetcher interface {
	FetchText(ctx context.Context, rawURL string) (title, text, final string, err error)
}

// localPageFetcher is the fetcher of web_fetch on this machine.
type localPageFetcher struct {
	docs documentFetcher
	html ingest.Extractor
	pdf  ingest.Extractor
}

// NewLocalPageFetcher is the local fetcher of web_fetch: any page with text is taken (not only an
// article, as for the library), with the same checks of the address as web_fetch.
func NewLocalPageFetcher() pageFetcher {
	return &localPageFetcher{
		docs: webfetch.New(webfetch.Options{
			MaxBytes:     webFetchMaxBytes,
			MinPageRunes: 1,
			UserAgent:    webFetchUserAgent,
			CheckURL: func(raw string) error {
				_, err := validateFetchURL(raw)
				return err
			},
		}),
		html: ingest.NewHTMLExtractor(),
		pdf:  ingest.NewPDFExtractor().WithoutOCR(),
	}
}

// errPageEmpty is a page that has (almost) no text.
var errPageEmpty = errors.New("the page has almost no text")

func (f *localPageFetcher) FetchText(ctx context.Context, rawURL string) (string, string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	doc, err := f.docs.Fetch(ctx, rawURL)
	if err != nil {
		return "", "", "", err
	}
	defer doc.Close()
	var extractor ingest.Extractor = f.html
	if doc.Kind == webfetch.KindPDF {
		extractor = f.pdf
	}
	text, err := extractor.Extract(ctx, doc.Body)
	if err != nil {
		if errors.Is(err, webfetch.ErrTooLarge) {
			return "", "", "", &webfetch.Error{Reason: webfetch.ReasonTooLarge, Detail: fmt.Sprintf("the document is larger than %d MB", webFetchMaxBytes>>20)}
		}
		return "", "", "", fmt.Errorf("%w: %v", errPageEmpty, err)
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) < webFetchMinRunes {
		return "", "", "", errPageEmpty
	}
	title := doc.Title
	if title == "" {
		title = strings.TrimSuffix(strings.TrimSuffix(doc.Name, ".pdf"), ".html")
	}
	return title, truncateRunes(text, webFetchMaxRunes), doc.Final, nil
}

// WithLocalWebFetch makes web_fetch read pages from this machine first.
func (r *DefaultToolRegistry) WithLocalWebFetch(f pageFetcher) *DefaultToolRegistry {
	r.pages = f
	return r
}

// fetchLocally is the local web_fetch: the result, or the reason it gave nothing (for the log
// and, when the paid fetch is not there to try, for the model).
func (r *DefaultToolRegistry) fetchLocally(ctx context.Context, pageURL string) (map[string]interface{}, string) {
	title, text, final, err := r.pages.FetchText(ctx, pageURL)
	if err != nil {
		reason := "failed"
		var fe *webfetch.Error
		switch {
		case errors.As(err, &fe):
			reason = string(fe.Reason)
		case errors.Is(err, errPageEmpty):
			reason = "empty"
		case errors.Is(err, context.DeadlineExceeded):
			reason = string(webfetch.ReasonTimeout)
		}
		metrics.WebToolRequests.WithLabelValues("web_fetch", "local_"+reason).Inc()
		slog.InfoContext(ctx, "the local web_fetch gave nothing", "reason", reason, "host", hostOf(pageURL), "error", err)
		return nil, reason
	}
	metrics.WebToolRequests.WithLabelValues("web_fetch", "local").Inc()
	result := map[string]interface{}{"title": title, "content": text}
	if final != "" && final != pageURL {
		result["url"] = final
	}
	return map[string]interface{}{
		"source":    "web",
		"untrusted": true,
		"notice":    webContentNotice,
		"fetched":   "local",
		"result":    result,
	}, ""
}

// localFetchReasons are what the model is told when the page could not be read and there is no
// paid fetch to try.
var localFetchReasons = map[string]string{
	string(webfetch.ReasonBlocked):     "the site refuses programs (401/403)",
	string(webfetch.ReasonRateLimited): "the site asks to wait (429)",
	string(webfetch.ReasonNotFound):    "the page does not exist (404)",
	string(webfetch.ReasonUnreachable): "the site could not be reached",
	string(webfetch.ReasonTimeout):     "the site did not answer in time",
	string(webfetch.ReasonTooLarge):    "the document is too large to read here",
	string(webfetch.ReasonUnsupported): "it is neither a web page nor a PDF",
	string(webfetch.ReasonNotPublic):   "the address is not a public one",
	string(webfetch.ReasonBadURL):      "the address is not a valid http or https link",
	"empty":                            "the page has no text that could be read (it may be made by JavaScript, or a scan)",
}

func localFetchError(reason string) error {
	why, ok := localFetchReasons[reason]
	if !ok {
		why = "the page could not be read"
	}
	return &toolArgumentError{tool: "web_fetch", reason: "the page was not read: " + why + ". Tell the user so; do not describe the page as if you had read it"}
}
