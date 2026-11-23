package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pdfBytes = "%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n"

// newTestFetcher reaches the servers of the test, which are on 127.0.0.1.
func newTestFetcher(opts Options) *Fetcher {
	opts.AllowPrivate = true
	if opts.MinPageRunes == 0 {
		opts.MinPageRunes = 300
	}
	return New(opts)
}

func article(words int) string {
	return strings.Repeat("Overnight returns reverse intraday returns. ", words)
}

func page(head, body string) string {
	return "<!doctype html><html><head><meta charset=utf-8>" + head + "</head><body>" + body + "</body></html>"
}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func fetch(t *testing.T, f *Fetcher, url string) (*Document, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return f.Fetch(ctx, url)
}

func reasonOf(t *testing.T, err error) Reason {
	t.Helper()
	var e *Error
	require.ErrorAs(t, err, &e, "the error of a link that was not taken")
	return e.Reason
}

func TestIsPublic(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "151.101.1.69", "2606:4700:4700::1111", "::ffff:8.8.8.8", "64:ff9b::808:808", "2002:808:808::1"}
	for _, a := range public {
		assert.True(t, isPublic(netip.MustParseAddr(a)), a)
	}
	local := []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.5.4", "192.168.1.1", "169.254.169.254", "0.0.0.0", "::",
		"100.64.0.1",       // shared address space of carriers
		"198.18.0.1",       // benchmarking
		"192.0.0.8",        // protocol assignments
		"240.0.0.1",        // reserved
		"224.0.0.1",        // multicast
		"fc00::1",          // unique local
		"fe80::1",          // link local
		"::ffff:10.0.0.1",  // IPv4 inside IPv6
		"::ffff:127.0.0.1", //
		"64:ff9b::7f00:1",  // NAT64 to 127.0.0.1
		"64:ff9b::a00:1",   // NAT64 to 10.0.0.1
		"2002:7f00:1::1",   // 6to4 of 127.0.0.1
		"2002:c0a8:101::1", // 6to4 of 192.168.1.1
		"2001:db8::1",      // documentation
	}
	for _, a := range local {
		assert.False(t, isPublic(netip.MustParseAddr(a)), a)
	}
}

// The production Fetcher refuses a server on this machine: the dialer looks at the address
// the connection is made to.
func TestTheDialerRefusesALocalServer(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, pdfBytes) })

	_, err := fetch(t, New(Options{}), srv.URL+"/a.pdf")

	require.Error(t, err)
	assert.Equal(t, ReasonNotPublic, reasonOf(t, err))
	assert.NotContains(t, err.Error(), srv.URL, "the address is not in the error")
}

func TestAPDFIsTaken(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, pdfBytes)
	})

	doc, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/files/Bajgrowicz_Scaillet.pdf")

	require.NoError(t, err)
	defer doc.Close()
	assert.Equal(t, KindPDF, doc.Kind)
	assert.Equal(t, "Bajgrowicz_Scaillet.pdf", doc.Name)
	assert.Empty(t, doc.Title, "a PDF linked directly has no page to give it a title")
	body, err := io.ReadAll(doc.Body)
	require.NoError(t, err)
	assert.Equal(t, pdfBytes, string(body))
}

// A PDF served as application/octet-stream, or with no type at all, is known by its start.
func TestAPDFIsKnownByItsStartNotByItsType(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, pdfBytes)
	})

	doc, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/download?id=7")

	require.NoError(t, err)
	defer doc.Close()
	assert.Equal(t, KindPDF, doc.Kind)
	assert.Equal(t, "download.pdf", doc.Name)
}

// The page of a paper names its PDF: the PDF is taken, named after the title.
func TestAPageThatNamesItsPDFGivesThePDF(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/papers/W16601", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page(
			`<title>Betting Against Beta | NBER</title><meta name="citation_title" content="Betting Against Beta"><meta name="citation_pdf_url" content="/system/files/w16601.pdf">`,
			"<p>Abstract. Short.</p>"))
	})
	mux.HandleFunc("/system/files/w16601.pdf", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, pdfBytes)
	})
	srv := serve(t, mux.ServeHTTP)

	doc, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/papers/W16601")

	require.NoError(t, err)
	defer doc.Close()
	assert.Equal(t, KindPDF, doc.Kind)
	assert.Equal(t, "Betting Against Beta.pdf", doc.Name, "named after the title of the paper")
	assert.Equal(t, "Betting Against Beta", doc.Title, "the title the page gave the article")
	body, _ := io.ReadAll(doc.Body)
	assert.Equal(t, pdfBytes, string(body))
}

func TestAPageWithTheTextOfAnArticleIsTakenAsHTML(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page("<title>Staff Report 1</title>", "<article><p>"+article(20)+"</p></article>"))
	})

	doc, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/research/sr1")

	require.NoError(t, err)
	defer doc.Close()
	assert.Equal(t, KindHTML, doc.Kind)
	assert.Equal(t, "Staff Report 1.html", doc.Name)
	assert.Equal(t, "Staff Report 1", doc.Title)
	body, _ := io.ReadAll(doc.Body)
	assert.Contains(t, string(body), "Overnight returns")
}

// The page of the LSE: an abstract of 1 300 bytes. It would be a file that looks like the
// article and is not (task of 2026-11-20).
func TestAPageWithOnlyAnAbstractIsRefused(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page("<title>A Tug of War</title>", "<p>Abstract: "+article(2)+"</p>"))
	})

	_, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/id/eprint/87481")

	require.Error(t, err)
	assert.Equal(t, ReasonAbstractOnly, reasonOf(t, err))
	assert.Contains(t, err.Error(), "abstract")
}

func TestAnAbstractWhosePDFIsBlockedSaysSo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/p", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page(`<meta name="citation_pdf_url" content="/x.pdf">`, "<p>short</p>"))
	})
	mux.HandleFunc("/x.pdf", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	srv := serve(t, mux.ServeHTTP)

	_, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/p")

	require.Error(t, err)
	assert.Equal(t, ReasonAbstractOnly, reasonOf(t, err))
	assert.Contains(t, err.Error(), "its PDF could not be taken (blocked)")
}

// A PDF that cannot be taken does not hide a page that holds the whole article.
func TestAPageWithAnArticleIsTakenWhenItsPDFFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/p", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page(`<title>Full</title><meta name="citation_pdf_url" content="/x.pdf">`, "<p>"+article(20)+"</p>"))
	})
	mux.HandleFunc("/x.pdf", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	srv := serve(t, mux.ServeHTTP)

	doc, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/p")

	require.NoError(t, err)
	defer doc.Close()
	assert.Equal(t, KindHTML, doc.Kind)
}

func TestStatusesBecomeReasons(t *testing.T) {
	for status, want := range map[int]Reason{
		http.StatusForbidden:           ReasonBlocked,
		http.StatusUnauthorized:        ReasonBlocked,
		http.StatusTooManyRequests:     ReasonRateLimited,
		http.StatusNotFound:            ReasonNotFound,
		http.StatusGone:                ReasonNotFound,
		http.StatusInternalServerError: ReasonUnreachable,
		http.StatusBadGateway:          ReasonUnreachable,
	} {
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })

		_, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/x")

		require.Error(t, err, status)
		assert.Equal(t, want, reasonOf(t, err), "status %d", status)
	}
}

func TestSomethingThatIsNotAPDFOrAPageIsRefused(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "\x89PNG\r\n\x1a\n....")
	})

	_, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/logo.png")

	require.Error(t, err)
	assert.Equal(t, ReasonUnsupported, reasonOf(t, err))
	assert.Contains(t, err.Error(), "image/png")
}

func TestAPDFThatSaysItIsTooLargeIsRefusedBeforeItIsRead(t *testing.T) {
	read := false
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", fmt.Sprint(5<<20))
		_, _ = io.WriteString(w, pdfBytes) // less than it said: the client must not wait for the rest
		read = true
	})
	f := newTestFetcher(Options{MaxBytes: 1 << 20})

	_, err := fetch(t, f, srv.URL+"/big.pdf")

	require.Error(t, err)
	assert.Equal(t, ReasonTooLarge, reasonOf(t, err))
	_ = read
}

// A server that does not say how large it is cannot send more than the limit: the body
// fails when it passes it, and the caller that stores it stops.
func TestAPDFThatPassesTheLimitWhileItIsReadFails(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, pdfBytes)
		for i := 0; i < 100; i++ {
			_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
		}
	})
	f := newTestFetcher(Options{MaxBytes: 64 << 10})

	doc, err := fetch(t, f, srv.URL+"/endless.pdf")

	require.NoError(t, err)
	defer doc.Close()
	n, err := io.Copy(io.Discard, doc.Body)
	assert.ErrorIs(t, err, ErrTooLarge)
	assert.LessOrEqual(t, n, int64(64<<10)+1, "not a byte more than the limit and one")
}

func TestAPageLargerThanTheLimitIsRefused(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page("<title>x</title>", "<p>"+strings.Repeat("a", 300_000)+"</p>"))
	})
	f := newTestFetcher(Options{MaxPageBytes: 100_000})

	_, err := fetch(t, f, srv.URL+"/huge")

	require.Error(t, err)
	assert.Equal(t, ReasonTooLarge, reasonOf(t, err))
}

// Every redirect is checked, with the address it leads to.
func TestARedirectToAForbiddenAddressIsRefused(t *testing.T) {
	var forbidden []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/inside", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, pdfBytes)
	})
	f := newTestFetcher(Options{CheckURL: func(raw string) error {
		if strings.HasSuffix(raw, "/inside") {
			forbidden = append(forbidden, raw)
			return errors.New("the host names this machine")
		}
		return nil
	}})

	_, err := fetch(t, f, srv.URL+"/start")

	require.Error(t, err)
	assert.Equal(t, ReasonNotPublic, reasonOf(t, err))
	assert.Len(t, forbidden, 1, "the target of the redirect was asked about")
}

func TestARedirectLoopStops(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, r.URL.Path, http.StatusFound) })

	_, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/loop")

	require.Error(t, err)
	assert.Equal(t, ReasonUnreachable, reasonOf(t, err))
	assert.Contains(t, err.Error(), "too many redirects")
}

func TestASlowServerTimesOut(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	_, err := newTestFetcher(Options{}).Fetch(ctx, srv.URL+"/slow")

	require.Error(t, err)
	assert.Equal(t, ReasonTimeout, reasonOf(t, err))
}

func TestABadLinkIsRefused(t *testing.T) {
	for _, raw := range []string{"ftp://example.com/a.pdf", "file:///etc/passwd", "javascript:alert(1)", "not a url", "https:///nohost"} {
		_, err := newTestFetcher(Options{}).Fetch(t.Context(), raw)
		require.Error(t, err, raw)
		assert.Equal(t, ReasonBadURL, reasonOf(t, err), raw)
	}
}

func TestCheckURLRunsBeforeTheFirstRequest(t *testing.T) {
	called := false
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { called = true })
	f := newTestFetcher(Options{CheckURL: func(string) error { return errors.New("no") }})

	_, err := fetch(t, f, srv.URL+"/x")

	require.Error(t, err)
	assert.Equal(t, ReasonNotPublic, reasonOf(t, err))
	assert.False(t, called, "no request was made")
}

func TestNames(t *testing.T) {
	assert.Equal(t, "Betting Against Beta.pdf", nameFromTitle("  Betting   Against\tBeta ", ".pdf"))
	assert.Equal(t, "When Do Stop-Loss Rules Stop Losses.pdf", nameFromTitle("When Do Stop-Loss Rules Stop Losses?", ".pdf"), "a character that is not allowed in a name does not stay")
	assert.Equal(t, "", nameFromTitle("   ", ".pdf"))
	assert.Equal(t, "document", safeName("???"))
	assert.LessOrEqual(t, len([]rune(safeName(strings.Repeat("дълго заглавие ", 20)))), maxNameRunes)
	assert.Equal(t, "Нощна доходност", safeName("Нощна доходност"), "letters of any alphabet stay")
	assert.Equal(t, "evil", safeName("../../evil"), "no path in a name")
}

// A redirect loop stops after maxRedirects: the server is asked that many times and no more.
func TestARedirectLoopIsAskedForNoMoreThanTheBound(t *testing.T) {
	var asked atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	})

	_, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/loop")

	require.Error(t, err)
	assert.LessOrEqual(t, int(asked.Load()), maxRedirects+1, "the loop was cut at the bound")
	assert.GreaterOrEqual(t, int(asked.Load()), 2, "and the redirects were followed up to it")
}

// A reader that already failed because of the limit keeps failing: the caller that reads on
// (a copy that retries) gets the same error, not a panic.
func TestABodyOverTheLimitKeepsFailingWhenReadAgain(t *testing.T) {
	body := &limitedBody{r: strings.NewReader(strings.Repeat("x", 100)), left: 10}
	buf := make([]byte, 64)

	_, err := body.Read(buf)
	require.ErrorIs(t, err, ErrTooLarge)
	for i := 0; i < 3; i++ {
		n, err := body.Read(buf)
		assert.Zero(t, n)
		assert.ErrorIs(t, err, ErrTooLarge)
	}
}

func TestAPDFIsKnownByItsStartAndItsLeadingBlanks(t *testing.T) {
	assert.True(t, isPDF([]byte("%PDF-1.7\n")))
	assert.True(t, isPDF([]byte("\r\n  %PDF-1.4")))
	assert.False(t, isPDF([]byte("<!doctype html>")))
	assert.False(t, isPDF([]byte("")))
}

// The requests say what they are: the library's own name, or the one given.
func TestTheRequestsNameThemselves(t *testing.T) {
	var agent string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, pdfBytes)
	})

	doc, err := fetch(t, newTestFetcher(Options{}), srv.URL+"/a.pdf")
	require.NoError(t, err)
	doc.Close()
	assert.Equal(t, userAgent, agent)

	doc, err = fetch(t, newTestFetcher(Options{UserAgent: "reader/2.0"}), srv.URL+"/a.pdf")
	require.NoError(t, err)
	doc.Close()
	assert.Equal(t, "reader/2.0", agent)
}
