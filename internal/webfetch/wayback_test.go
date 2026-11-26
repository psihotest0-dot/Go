package webfetch

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWayback is the Wayback Machine of a test: its index answers with copies (the link they were
// kept under -> timestamp), and the raw copies are pages (by timestamp + "id_/" + link).
type fakeWayback struct {
	copies map[string]string // original link -> timestamp
	pages  map[string]string // "<timestamp>id_/<link>" -> body
	gzip   bool              // send the copies compressed, as the archive does
	asked  []string          // the paths asked for, in order
}

func (w *fakeWayback) serve(t *testing.T) string {
	t.Helper()
	srv := serve(t, func(rw http.ResponseWriter, r *http.Request) {
		w.asked = append(w.asked, r.URL.Path)
		if r.URL.Path == "/cdx/search/cdx" {
			assert.Equal(t, "statuscode:200", r.URL.Query().Get("filter"))
			assert.Equal(t, "-1", r.URL.Query().Get("limit"))
			link := r.URL.Query().Get("url")
			stamp, ok := w.copies[link]
			if !ok {
				_, _ = io.WriteString(rw, "[]")
				return
			}
			_, _ = io.WriteString(rw, `[["timestamp","original"],["`+stamp+`","`+link+`"]]`)
			return
		}
		key := strings.TrimPrefix(r.URL.RequestURI(), "/web/")
		body, ok := w.pages[key]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		if w.gzip {
			rw.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(rw)
			_, _ = io.WriteString(zw, body)
			_ = zw.Close()
			return
		}
		_, _ = io.WriteString(rw, body)
	})
	return srv.URL
}

func archived(t *testing.T, f *Fetcher, link string) (*Document, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return f.Archived(ctx, link)
}

func TestTheNewestCopyOfAPageIsTaken(t *testing.T) {
	link := "https://example.org/blog/overnight-returns"
	w := &fakeWayback{
		copies: map[string]string{link: "20260716080910"},
		pages:  map[string]string{"20260716080910id_/" + link: page("<title>Overnight</title>", "<article>"+article(40)+"</article>")},
		gzip:   true,
	}
	f := newTestFetcher(Options{WaybackURL: w.serve(t)})

	doc, err := archived(t, f, link)
	require.NoError(t, err)
	defer doc.Close()
	assert.Equal(t, KindHTML, doc.Kind)
	assert.Equal(t, time.Date(2026, 7, 16, 8, 9, 10, 0, time.UTC), doc.Archived)
	assert.Contains(t, doc.Final, "/web/20260716080910id_/"+link, "the address of the copy")
	body, _ := io.ReadAll(doc.Body)
	assert.Contains(t, string(body), "Overnight returns reverse", "the copy is read decompressed")
}

func TestAPageWithoutACopySaysSo(t *testing.T) {
	w := &fakeWayback{}
	f := newTestFetcher(Options{WaybackURL: w.serve(t)})
	_, err := archived(t, f, "https://example.org/never-kept")
	assert.Equal(t, ReasonNotArchived, reasonOf(t, err))
}

// The PDF a copied page names is taken from the archive, not from the site that refused.
func TestTheCopyOfAPageGivesTheCopyOfItsPDF(t *testing.T) {
	link := "https://papers.example.org/abstract=1"
	pdf := "https://papers.example.org/delivery/1.pdf"
	w := &fakeWayback{
		copies: map[string]string{link: "20261112204431"},
		pages: map[string]string{
			"20261112204431id_/" + link: page(`<meta name="citation_title" content="Day Trading"><meta name="citation_pdf_url" content="/delivery/1.pdf">`, "<p>Abstract only.</p>"),
			"20261112204431id_/" + pdf:  pdfBytes,
		},
	}
	f := newTestFetcher(Options{WaybackURL: w.serve(t)})

	doc, err := archived(t, f, link)
	require.NoError(t, err)
	defer doc.Close()
	assert.Equal(t, KindPDF, doc.Kind)
	assert.Equal(t, "Day Trading", doc.Title)
	assert.False(t, doc.Archived.IsZero())
	assert.Equal(t, []string{"/cdx/search/cdx", "/web/20261112204431id_/" + link, "/web/20261112204431id_/" + pdf}, w.asked)
}

func TestTheCopyOfAnAbstractIsRefusedLikeThePageItself(t *testing.T) {
	link := "https://example.org/abstract"
	w := &fakeWayback{
		copies: map[string]string{link: "20250101000000"},
		pages:  map[string]string{"20250101000000id_/" + link: page("", "<p>Abstract only.</p>")},
	}
	f := newTestFetcher(Options{WaybackURL: w.serve(t)})
	_, err := archived(t, f, link)
	assert.Equal(t, ReasonAbstractOnly, reasonOf(t, err))
	assert.Contains(t, err.Error(), "Wayback Machine")
}

func TestAnArchiveThatCannotBeAskedSaysSo(t *testing.T) {
	srv := serve(t, func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusServiceUnavailable) })
	f := newTestFetcher(Options{WaybackURL: srv.URL})
	_, err := archived(t, f, "https://example.org/page")
	assert.Equal(t, ReasonUnreachable, reasonOf(t, err))
	assert.Contains(t, err.Error(), "the Wayback Machine could not be asked")

	bad := serve(t, func(rw http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(rw, "<html>maintenance</html>") })
	_, err = archived(t, newTestFetcher(Options{WaybackURL: bad.URL}), "https://example.org/page")
	assert.Equal(t, ReasonUnreachable, reasonOf(t, err))
}

// The link itself must pass the checks of the fetcher before the archive is asked about it.
func TestALinkThatIsRefusedIsNotAskedAbout(t *testing.T) {
	w := &fakeWayback{}
	f := newTestFetcher(Options{WaybackURL: w.serve(t), CheckURL: func(raw string) error {
		if u, err := url.Parse(raw); err != nil || u.Hostname() == "intranet.local" {
			return assert.AnError
		}
		return nil
	}})
	_, err := archived(t, f, "http://intranet.local/secret")
	assert.Equal(t, ReasonNotPublic, reasonOf(t, err))
	_, err = archived(t, f, "ftp://example.org/file")
	assert.Equal(t, ReasonBadURL, reasonOf(t, err))
	assert.Empty(t, w.asked)
}

func TestTheDefaultArchiveIsTheWaybackMachine(t *testing.T) {
	assert.Equal(t, DefaultWaybackURL, New(Options{}).wayback)
	assert.Equal(t, "http://x", New(Options{WaybackURL: "http://x/"}).wayback)
}
