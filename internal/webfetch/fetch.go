// Package webfetch downloads a document that a user pointed to - a PDF, or an article on a
// web page - so that it can be added to the library (plan item P53).
//
// It is not the web_fetch tool of the chat. That one is made by a paid API, gives the model
// the text of the page and cannot read a PDF. This one is made from this machine, never
// shows the text to the model, and takes only what the ingest can read.
//
// The address comes from a person and the request is made from the server, so every
// connection - the first and every redirect - is checked against the address it is really
// made to (publicOnly), not only against the name in the link.
package webfetch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"go-monolith/internal/ingest"
)

const (
	// DefaultMaxBytes is the largest document taken (a PDF; a page is cut earlier).
	DefaultMaxBytes int64 = 50 << 20
	// defaultMaxPageBytes is the largest HTML page read. The page is read whole, to find
	// the link to its PDF and to count its text; real articles are far smaller.
	defaultMaxPageBytes int64 = 8 << 20
	// DefaultMinPageRunes: a page with less text than this is an abstract or a landing page
	// ("Lou, Polk, Skouras" at the LSE: 1 300 bytes), not an article.
	DefaultMinPageRunes = 6000

	maxRedirects    = 5
	maxNameRunes    = 80
	headBytesForPDF = 1024
	maxHeadBytes    = 512 << 10
	userAgent       = "go-monolith-library/1.0 (a person's own assistant; one request per link the person wrote)"
)

// Reason says why a link was not added, in words for a log and a table.
type Reason string

const (
	ReasonBlocked      Reason = "blocked"       // 401, 403: the site refuses a program
	ReasonRateLimited  Reason = "rate_limited"  // 429: try later
	ReasonNotFound     Reason = "not_found"     // 404, 410
	ReasonUnreachable  Reason = "unreachable"   // no connection, 5xx, other statuses
	ReasonTimeout      Reason = "timeout"       //
	ReasonTooLarge     Reason = "too_large"     //
	ReasonUnsupported  Reason = "unsupported"   // neither a PDF nor an HTML page
	ReasonAbstractOnly Reason = "abstract_only" // an HTML page with little text and no PDF behind it
	ReasonNotPublic    Reason = "not_public"    // the address is a local or private one
	ReasonBadURL       Reason = "bad_url"       //
)

// Error is a link that could not be taken. Detail is short and never holds the address.
type Error struct {
	Reason Reason
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Reason)
	}
	return string(e.Reason) + ": " + e.Detail
}

// ErrTooLarge is what the body of a document returns when it passes the limit while it is
// being read (the check before the read is the Content-Length one).
var ErrTooLarge = errors.New("the document is larger than the limit")

// Kind is the kind of document that was taken.
type Kind string

const (
	KindPDF  Kind = "pdf"
	KindHTML Kind = "html"
)

// Document is a downloaded document. Body is read once, by the caller; Close releases the
// connection and must be called.
type Document struct {
	Name string
	Kind Kind
	Body io.Reader
	// Final is the address the document was taken from, for the log (host only is logged).
	Final string

	closer io.Closer
}

// Close releases the connection of a PDF that was not read to its end.
func (d *Document) Close() {
	if d != nil && d.closer != nil {
		_ = d.closer.Close()
	}
}

// Options bound a Fetcher. The zero value is the production setting.
type Options struct {
	MaxBytes     int64
	MaxPageBytes int64
	MinPageRunes int
	// Timeout bounds one request (headers and, for a page, the body). A PDF body is read
	// later by the caller, under the context given to Fetch.
	Timeout time.Duration
	// CheckURL is called with every address before a request, and with the target of every
	// redirect; an error refuses it. The names that are not public ones are refused here
	// (the addresses are refused by the dialer).
	CheckURL func(rawURL string) error
	// AllowPrivate lets the dialer connect to private addresses. Tests only.
	AllowPrivate bool
}

// Fetcher downloads documents. It is safe for concurrent use.
type Fetcher struct {
	client       *http.Client
	maxBytes     int64
	maxPageBytes int64
	minPageRunes int
	checkURL     func(string) error
}

// New builds a Fetcher.
func New(opts Options) *Fetcher {
	f := &Fetcher{
		maxBytes:     opts.MaxBytes,
		maxPageBytes: opts.MaxPageBytes,
		minPageRunes: opts.MinPageRunes,
		checkURL:     opts.CheckURL,
	}
	if f.maxBytes <= 0 {
		f.maxBytes = DefaultMaxBytes
	}
	if f.maxPageBytes <= 0 {
		f.maxPageBytes = defaultMaxPageBytes
	}
	if f.minPageRunes <= 0 {
		f.minPageRunes = DefaultMinPageRunes
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !opts.AllowPrivate {
		dialer.Control = publicOnly
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{
		// No proxy: a proxy named in the environment would be the address the dialer sees,
		// and the check would say nothing about the site behind it.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	f.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return &Error{Reason: ReasonUnreachable, Detail: "too many redirects"}
			}
			return f.allowed(req.URL.String())
		},
	}
	return f
}

// allowed runs the CheckURL of the Fetcher on an address.
func (f *Fetcher) allowed(raw string) error {
	if f.checkURL == nil {
		return nil
	}
	if err := f.checkURL(raw); err != nil {
		return &Error{Reason: ReasonNotPublic, Detail: err.Error()}
	}
	return nil
}

// Fetch downloads the document at rawURL:
//
//   - a PDF is taken as it is;
//   - a page that names its PDF (<meta name="citation_pdf_url">, which the journals and
//     working paper series put on the page of an article) gives that PDF;
//   - a page with the text of an article (at least MinPageRunes of it) is taken as HTML;
//   - a page with less text is refused: it is the abstract, and the library would hold a
//     file that looks like the article and is not.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Document, error) {
	if err := f.allowed(rawURL); err != nil {
		return nil, err
	}
	resp, err := f.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	body := bufio.NewReaderSize(resp.Body, 8192)
	head, _ := body.Peek(headBytesForPDF)

	if isPDF(head) {
		return f.pdfDocument(resp, body, "")
	}
	if !isHTML(resp.Header.Get("Content-Type"), head) {
		_ = resp.Body.Close()
		return nil, &Error{Reason: ReasonUnsupported, Detail: "neither a PDF nor a web page (" + shortType(resp.Header.Get("Content-Type")) + ")"}
	}

	page, err := readLimited(body, f.maxPageBytes)
	_ = resp.Body.Close()
	if err != nil {
		return nil, bodyError(ctx, err, "the page")
	}
	meta := readMeta(page)
	final := resp.Request.URL

	var pdfFailure *Error
	if meta.pdfURL != "" {
		target, err := final.Parse(meta.pdfURL)
		if err == nil && (target.Scheme == "http" || target.Scheme == "https") {
			doc, perr := f.fetchPDF(ctx, target.String(), nameFromTitle(meta.title, ".pdf"))
			if perr == nil {
				return doc, nil
			}
			var e *Error
			if errors.As(perr, &e) {
				pdfFailure = e
			}
		}
	}

	text, err := ingest.NewHTMLExtractor().Extract(ctx, bytes.NewReader(page))
	if err != nil {
		return nil, &Error{Reason: ReasonUnsupported, Detail: "the page could not be read as HTML"}
	}
	if n := utf8.RuneCountInString(text); n < f.minPageRunes {
		detail := fmt.Sprintf("the page holds only %d characters of text (an abstract or a landing page), not the article", n)
		if pdfFailure != nil {
			detail += "; its PDF could not be taken (" + string(pdfFailure.Reason) + ")"
		}
		return nil, &Error{Reason: ReasonAbstractOnly, Detail: detail}
	}
	name := nameFromTitle(meta.title, ".html")
	if name == "" {
		name = nameFromURL(final, ".html")
	}
	return &Document{Name: name, Kind: KindHTML, Body: bytes.NewReader(page), Final: final.String()}, nil
}

// fetchPDF downloads a PDF that a page named.
func (f *Fetcher) fetchPDF(ctx context.Context, rawURL, name string) (*Document, error) {
	if err := f.allowed(rawURL); err != nil {
		return nil, err
	}
	resp, err := f.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	body := bufio.NewReaderSize(resp.Body, 8192)
	head, _ := body.Peek(headBytesForPDF)
	if !isPDF(head) {
		_ = resp.Body.Close()
		return nil, &Error{Reason: ReasonUnsupported, Detail: "the link that should hold the PDF does not"}
	}
	return f.pdfDocument(resp, body, name)
}

// pdfDocument wraps the body of a response that is a PDF. The body is limited while it is
// read: a server that does not say how large it is cannot send more than the limit.
func (f *Fetcher) pdfDocument(resp *http.Response, body *bufio.Reader, name string) (*Document, error) {
	if resp.ContentLength > f.maxBytes {
		_ = resp.Body.Close()
		return nil, &Error{Reason: ReasonTooLarge, Detail: fmt.Sprintf("the PDF is %d MB; the limit is %d MB", resp.ContentLength>>20, f.maxBytes>>20)}
	}
	if name == "" {
		name = nameFromURL(resp.Request.URL, ".pdf")
	}
	return &Document{
		Name:   name,
		Kind:   KindPDF,
		Body:   &limitedBody{r: body, left: f.maxBytes},
		Final:  resp.Request.URL.String(),
		closer: resp.Body,
	}, nil
}

// get makes the request and turns what is not a 200 into an Error.
func (f *Fetcher) get(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, &Error{Reason: ReasonBadURL, Detail: "not an http or https link"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &Error{Reason: ReasonBadURL, Detail: "not an http or https link"}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/pdf, text/html;q=0.9, */*;q=0.1")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, requestError(ctx, err)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		_ = resp.Body.Close()
		return nil, &Error{Reason: ReasonBlocked, Detail: fmt.Sprintf("the site answered %d", resp.StatusCode)}
	case resp.StatusCode == http.StatusTooManyRequests:
		_ = resp.Body.Close()
		return nil, &Error{Reason: ReasonRateLimited, Detail: "the site answered 429; try again later"}
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		_ = resp.Body.Close()
		return nil, &Error{Reason: ReasonNotFound, Detail: fmt.Sprintf("the site answered %d", resp.StatusCode)}
	}
	_ = resp.Body.Close()
	return nil, &Error{Reason: ReasonUnreachable, Detail: fmt.Sprintf("the site answered %d", resp.StatusCode)}
}

// requestError maps a failure of the client, without the address (a url.Error carries it).
func requestError(ctx context.Context, err error) error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, errNotPublic):
		return &Error{Reason: ReasonNotPublic, Detail: "the link leads to a local or private address"}
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return &Error{Reason: ReasonTimeout, Detail: "the site did not answer in time"}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Reason: ReasonTimeout, Detail: "the site did not answer in time"}
	}
	return &Error{Reason: ReasonUnreachable, Detail: "no connection to the site"}
}

func bodyError(ctx context.Context, err error, what string) error {
	switch {
	case errors.Is(err, ErrTooLarge):
		return &Error{Reason: ReasonTooLarge, Detail: what + " is larger than the limit"}
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return &Error{Reason: ReasonTimeout, Detail: "the site did not finish sending " + what + " in time"}
	}
	return &Error{Reason: ReasonUnreachable, Detail: "the connection broke while " + what + " was sent"}
}

// limitedBody is a reader that fails with ErrTooLarge instead of ending when the document
// is larger than the limit.
type limitedBody struct {
	r    io.Reader
	left int64
}

func (l *limitedBody) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

// readLimited reads r whole, and fails with ErrTooLarge when it is longer than limit.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

func isPDF(head []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n"), []byte("%PDF-"))
}

func isHTML(contentType string, head []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	sniffed := strings.ToLower(strings.TrimSpace(string(head[:min(len(head), 512)])))
	return strings.HasPrefix(sniffed, "<!doctype html") || strings.HasPrefix(sniffed, "<html")
}

func shortType(contentType string) string {
	contentType, _, _ = strings.Cut(contentType, ";")
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return "no content type"
	}
	if len(contentType) > 40 {
		contentType = contentType[:40]
	}
	return contentType
}

// pageMeta is what the head of a page says about the article.
type pageMeta struct {
	pdfURL string
	title  string
}

// readMeta reads citation_pdf_url, citation_title and <title> from the head of a page.
func readMeta(page []byte) pageMeta {
	if len(page) > maxHeadBytes {
		page = page[:maxHeadBytes]
	}
	var meta pageMeta
	var fallbackTitle string
	z := html.NewTokenizer(bytes.NewReader(page))
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			if meta.title == "" {
				meta.title = fallbackTitle
			}
			return meta
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			switch tok.Data {
			case "body":
				if meta.title == "" {
					meta.title = fallbackTitle
				}
				return meta
			case "title":
				inTitle = tok.Type == html.StartTagToken
			case "meta":
				var name, content string
				for _, a := range tok.Attr {
					switch strings.ToLower(a.Key) {
					case "name", "property":
						name = strings.ToLower(a.Val)
					case "content":
						content = a.Val
					}
				}
				switch name {
				case "citation_pdf_url":
					if meta.pdfURL == "" {
						meta.pdfURL = strings.TrimSpace(content)
					}
				case "citation_title":
					if meta.title == "" {
						meta.title = strings.TrimSpace(content)
					}
				}
			}
		case html.TextToken:
			if inTitle && fallbackTitle == "" {
				fallbackTitle = strings.TrimSpace(string(z.Text()))
			}
		case html.EndTagToken:
			if z.Token().Data == "title" {
				inTitle = false
			}
		}
	}
}

// nameFromTitle makes a file name from the title of an article, or "" when there is none.
func nameFromTitle(title, ext string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return ""
	}
	return safeName(title) + ext
}

// nameFromURL makes a file name from the last part of the path of an address, or from its
// host when the path has none.
func nameFromURL(u *url.URL, ext string) string {
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return safeName(u.Hostname()) + ext
	}
	base = strings.TrimSuffix(base, path.Ext(base))
	if base == "" {
		return safeName(u.Hostname()) + ext
	}
	return safeName(base) + ext
}

// safeName keeps letters, digits and a few marks; the rest becomes "_". The ingest keeps
// the name as the user will see it in the library, so it is not made ugly: the spaces stay.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r), strings.ContainsRune(" -_.,()", r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(strings.TrimSpace(b.String()), "._ ")
	if runes := []rune(name); len(runes) > maxNameRunes {
		name = strings.TrimSpace(string(runes[:maxNameRunes]))
	}
	if name == "" {
		return "document"
	}
	return name
}
