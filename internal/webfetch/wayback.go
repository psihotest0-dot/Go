package webfetch

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

// The Wayback Machine of the Internet Archive as the last road to a page (2026-11-26, the owner:
// "направи само wayback"): a link that answers 404, refuses a program, or does not answer is read
// from the newest copy the archive kept of it. Measured on the 23 links the owner had written
// (go-monolith-backups\measurements\wayback_2026-11-26): 16 have a copy, both links that refused a
// program (403) were read from theirs. The copy is the raw one ("<timestamp>id_/"): the page as the
// site sent it, without the bar of the archive, so the extractors see what they see on the site.
// Nothing is sent but the link itself, and no key is needed.

const (
	// DefaultWaybackURL is the address of the Wayback Machine.
	DefaultWaybackURL = "https://web.archive.org"
	// cdxMaxBytes bounds the answer of the index (one row is asked for).
	cdxMaxBytes = 64 << 10
	// waybackStamp is the layout of the timestamps of the archive (UTC).
	waybackStamp = "20060102150405"
)

// ReasonNotArchived: the Wayback Machine has no copy of the link with status 200.
const ReasonNotArchived Reason = "not_archived"

// Archived downloads the newest copy of rawURL that the Wayback Machine kept with status 200, the
// way Fetch downloads the page itself: a PDF as it is, the PDF a page names (its copy in the
// archive), or the text of an article. The Document has the day of the copy in Archived and the
// address of the copy in Final. ReasonNotArchived when the archive has no copy; when the archive
// itself cannot be asked, the Error says so in its Detail.
func (f *Fetcher) Archived(ctx context.Context, rawURL string) (*Document, error) {
	original, err := url.Parse(rawURL)
	if err != nil || (original.Scheme != "http" && original.Scheme != "https") || original.Hostname() == "" {
		return nil, &Error{Reason: ReasonBadURL, Detail: "not an http or https link"}
	}
	if err := f.allowed(rawURL); err != nil {
		return nil, err
	}
	stamp, kept, err := f.newestCopy(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	at, err := time.Parse(waybackStamp, stamp)
	if err != nil {
		return nil, &Error{Reason: ReasonUnreachable, Detail: "the Wayback Machine gave a copy without a date"}
	}
	copyOf := func(link string) string { return f.wayback + "/web/" + stamp + "id_/" + link }
	// The PDF a copied page names is the PDF of the original site: its copy is asked for (the
	// archive answers with its nearest copy, or 404), never the site itself.
	pdfAt := func(_ *url.URL, named string) (string, bool) {
		target, err := original.Parse(named)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
			return "", false
		}
		return copyOf(target.String()), true
	}
	doc, err := f.fetch(ctx, copyOf(kept), pdfAt)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, &Error{Reason: e.Reason, Detail: "the copy in the Wayback Machine: " + e.Detail}
		}
		return nil, err
	}
	doc.Archived = at
	return doc, nil
}

// newestCopy asks the index of the archive (the CDX API) for the newest copy of rawURL with status
// 200: its timestamp and the address it was kept under.
func (f *Fetcher) newestCopy(ctx context.Context, rawURL string) (stamp, kept string, err error) {
	q := url.Values{
		"url":    {rawURL},
		"output": {"json"},
		"fl":     {"timestamp,original"},
		"filter": {"statuscode:200"},
		"limit":  {"-1"}, // the last row: the newest copy
	}
	index := f.wayback + "/cdx/search/cdx?" + q.Encode()
	if err := f.allowed(index); err != nil {
		return "", "", err
	}
	resp, err := f.get(ctx, index)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return "", "", &Error{Reason: e.Reason, Detail: "the Wayback Machine could not be asked: " + e.Detail}
		}
		return "", "", err
	}
	raw, err := readLimited(resp.Body, cdxMaxBytes)
	_ = resp.Body.Close()
	if err != nil {
		return "", "", &Error{Reason: ReasonUnreachable, Detail: "the Wayback Machine could not be asked: its answer broke off"}
	}
	var rows [][]string
	if strings.TrimSpace(string(raw)) == "" {
		rows = nil
	} else if err := json.Unmarshal(raw, &rows); err != nil {
		return "", "", &Error{Reason: ReasonUnreachable, Detail: "the Wayback Machine gave an answer that could not be read"}
	}
	// The first row names the fields.
	if len(rows) < 2 || len(rows[len(rows)-1]) < 2 {
		return "", "", &Error{Reason: ReasonNotArchived, Detail: "the Wayback Machine has no copy of the page"}
	}
	last := rows[len(rows)-1]
	kept = last[1]
	if u, err := url.Parse(kept); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		kept = rawURL
	}
	return last[0], kept, nil
}
