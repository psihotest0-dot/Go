package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// The Wayback Machine as the last road of web_fetch, save_page and add_to_library (WAYBACK_ENABLED,
// 2026-11-26; webfetch/wayback.go). A link that is gone (404), refuses a program (403), asks to wait
// (429) or does not answer is read from the newest copy the archive kept of it, and the model and
// the person are told that it is a copy and of which day. A page gone for this machine is gone for
// the paid API too, so after a 404 the copy is tried before the paid fetch; after the other reasons
// the paid fetch, which reads the page as it is now, goes first. No tool is added: the model calls
// the tools it called before.

// archiveFetcher downloads the newest copy of a link the Wayback Machine kept (webfetch.Fetcher).
type archiveFetcher interface {
	Archived(ctx context.Context, rawURL string) (*webfetch.Document, error)
}

// archivedPages gives the text of such a copy (localPageFetcher).
type archivedPages interface {
	ArchivedText(ctx context.Context, rawURL string) (title, text, final string, at time.Time, err error)
}

// WithWayback lets web_fetch, save_page and add_to_library read a link from its copy in the
// Wayback Machine when the link itself cannot be read.
func (r *DefaultToolRegistry) WithWayback() *DefaultToolRegistry {
	r.wayback = true
	return r
}

// archivableReasons are the reasons a link is looked for in the archive. A page with no text (made
// by JavaScript) is the same in its copy; a link that is not public, or not a link, is not asked
// about; an abstract is not the article in the archive either.
var archivableReasons = map[string]bool{
	string(webfetch.ReasonNotFound):    true,
	string(webfetch.ReasonBlocked):     true,
	string(webfetch.ReasonRateLimited): true,
	string(webfetch.ReasonUnreachable): true,
	string(webfetch.ReasonTimeout):     true,
}

// errNoArchive: the fetcher of the pages cannot ask the archive.
var errNoArchive = errors.New("the fetcher has no archive")

// archivedNote is what the model is told next to the text of a copy: reason is why the page itself
// was not read.
func archivedNote(at time.Time, reason string) string {
	why, ok := localFetchReasons[reason]
	if !ok {
		why = "the page could not be read"
	}
	return fmt.Sprintf("The page itself could not be read (%s). This text is from the copy the Wayback Machine (web.archive.org) kept on %s. "+
		"Tell the user that it is that archived copy and give its date: the page may have changed or be gone since.", why, at.Format("2006-01-02"))
}

// archivedPage is the text of the copy of pageURL in the archive, for web_fetch and save_page; ok is
// false when the archive is off, the reason is not one to look for a copy for, or there is no copy.
func (r *DefaultToolRegistry) archivedPage(ctx context.Context, tool, pageURL, reason string) (title, text, final, note string, ok bool) {
	if !r.wayback || !archivableReasons[reason] {
		return "", "", "", "", false
	}
	pages, isArchive := r.pages.(archivedPages)
	if !isArchive {
		return "", "", "", "", false
	}
	title, text, final, at, err := pages.ArchivedText(ctx, pageURL)
	if err != nil {
		why := localFailure(err)
		metrics.WebToolRequests.WithLabelValues(tool, "wayback_"+why).Inc()
		slog.InfoContext(ctx, "the copy in the Wayback Machine gave nothing", "tool", tool, "reason", why, "host", hostOf(pageURL), "error", err)
		return "", "", "", "", false
	}
	metrics.WebToolRequests.WithLabelValues(tool, "wayback").Inc()
	slog.InfoContext(ctx, "a page was read from its copy in the Wayback Machine", "tool", tool, "because", reason, "host", hostOf(pageURL), "copy_of", at.Format("2006-01-02"))
	return title, text, final, archivedNote(at, reason), true
}

// archivedFetchResult is the result of web_fetch made from the copy of pageURL, nil when there is none.
func (r *DefaultToolRegistry) archivedFetchResult(ctx context.Context, pageURL, reason string) map[string]interface{} {
	title, text, final, note, ok := r.archivedPage(ctx, "web_fetch", pageURL, reason)
	if !ok {
		return nil
	}
	return map[string]interface{}{
		"source":        "web",
		"untrusted":     true,
		"notice":        webContentNotice,
		"fetched":       "wayback",
		"archived_copy": note,
		"result":        map[string]interface{}{"title": title, "content": truncateRunes(text, webFetchMaxRunes), "url": final},
	}
}

// archivedDocument is the copy in the archive of a link add_to_library could not take (err), nil
// when there is none.
func (r *DefaultToolRegistry) archivedDocument(ctx context.Context, link string, err error) (*webfetch.Document, string) {
	var fe *webfetch.Error
	if !r.wayback || !errors.As(err, &fe) || !archivableReasons[string(fe.Reason)] {
		return nil, ""
	}
	archive, ok := r.fetcher.(archiveFetcher)
	if !ok {
		return nil, ""
	}
	doc, aerr := archive.Archived(ctx, link)
	if aerr != nil {
		why := localFailure(aerr)
		metrics.LibraryAdd.WithLabelValues("wayback_" + why).Inc()
		slog.InfoContext(ctx, "library add: the copy in the Wayback Machine gave nothing", "host", hostOf(link), "reason", why, "error", aerr)
		return nil, ""
	}
	slog.InfoContext(ctx, "library add: taken from its copy in the Wayback Machine", "host", hostOf(link), "because", fe.Reason, "copy_of", doc.Archived.Format("2006-01-02"))
	return doc, string(fe.Reason)
}

// ArchivedText is FetchText for the newest copy of rawURL in the Wayback Machine.
func (f *localPageFetcher) ArchivedText(ctx context.Context, rawURL string) (string, string, string, time.Time, error) {
	archive, ok := f.docs.(archiveFetcher)
	if !ok {
		return "", "", "", time.Time{}, errNoArchive
	}
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	doc, err := archive.Archived(ctx, rawURL)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	defer doc.Close()
	title, text, err := f.textOf(ctx, doc)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return title, text, doc.Final, doc.Archived, nil
}
