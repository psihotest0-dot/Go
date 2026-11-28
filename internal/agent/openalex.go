package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"go-monolith/metrics"
)

// The works of a reference are looked up in OpenAlex (api.openalex.org), a free catalogue of scholarly
// works, before the general search (Р3, 2026-11-28, the owner: "опитай да подобриш търсенето"). On the
// real list of 2026-11-20 the general search gave, for a title or for names, sentences from inside
// other PDFs that cite the work; the catalogue gives the work itself with its authors, its year and
// the addresses of its free PDFs (a working paper of NBER, a copy in a university repository). It
// finds a reference that has only names and a year as well: "Sullivan, Timmermann, White (1999)" is
// "Data-Snooping, Technical Trading Rule Performance, and the Bootstrap".
//
// What is sent: the title of the reference, or the surnames and the year, as written in the person's
// list - nothing else, and no address or name of the person (OpenAlex asks for an e-mail for its
// faster pool; none is given). Only metadata comes back; no document is opened before the yes.

const (
	defaultOpenAlexURL = "https://api.openalex.org/works"
	// openAlexWorks is how many works a lookup reads.
	openAlexWorks = 8
	// maxOpenAlexBytes bounds the answer of a lookup (eight works with their locations are ~60 KB).
	maxOpenAlexBytes = 1 << 20
	// openAlexAgent is how the lookup names itself.
	openAlexAgent = "go-monolith-assistant/1.0"
)

// scholarWork is a work of the catalogue: its title, year, the surnames of its authors and the
// addresses of its free PDFs, the best one first.
type scholarWork struct {
	Title    string
	Year     int
	Surnames []string
	PDFs     []string
	// Open: the catalogue knows a free version of it somewhere (open_access.is_oa), even one the code
	// cannot fetch (SSRN).
	Open bool
}

// scholarSearcher looks up the works of a reference (DefaultToolRegistry).
type scholarSearcher interface {
	scholarWorks(ctx context.Context, ref reference) ([]scholarWork, error)
}

// openAlexQuery is the address of the lookup of a reference: its title, or (without one) the surnames
// of its authors with its year; "" when there is not enough to look for.
func (r *DefaultToolRegistry) openAlexQuery(ref reference) string {
	base := r.openAlexURL
	if base == "" {
		base = defaultOpenAlexURL
	}
	v := url.Values{}
	v.Set("per-page", strconv.Itoa(openAlexWorks))
	if ref.Title != "" {
		v.Set("search", ref.Title)
		return base + "?" + v.Encode()
	}
	names := surnames(ref)
	if len(names) < 2 {
		return ""
	}
	var filters []string
	for _, n := range names {
		n = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || r == '-' || r == '\'' || r == '’' {
				return r
			}
			return -1
		}, n)
		if n != "" {
			filters = append(filters, "raw_author_name.search:"+n)
		}
	}
	if ref.Year != "" {
		filters = append(filters, "publication_year:"+ref.Year)
	}
	v.Set("filter", strings.Join(filters, ","))
	v.Set("sort", "cited_by_count:desc")
	return base + "?" + v.Encode()
}

// scholarWorks looks the reference up in the catalogue.
func (r *DefaultToolRegistry) scholarWorks(ctx context.Context, ref reference) ([]scholarWork, error) {
	query := r.openAlexQuery(ref)
	if query == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, query, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", openAlexAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		metrics.WebToolRequests.WithLabelValues("openalex", "error").Inc()
		return nil, fmt.Errorf("the catalogue could not be reached: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOpenAlexBytes))
	if err != nil {
		metrics.WebToolRequests.WithLabelValues("openalex", "error").Inc()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		metrics.WebToolRequests.WithLabelValues("openalex", "error").Inc()
		slog.WarnContext(ctx, "the catalogue answered with an error", "status", resp.StatusCode)
		return nil, fmt.Errorf("the catalogue answered %d", resp.StatusCode)
	}
	var answer struct {
		Results []struct {
			Title       string `json:"title"`
			Year        int    `json:"publication_year"`
			Authorships []struct {
				Author struct {
					Name string `json:"display_name"`
				} `json:"author"`
			} `json:"authorships"`
			Best *struct {
				PDF string `json:"pdf_url"`
			} `json:"best_oa_location"`
			Access struct {
				IsOA bool `json:"is_oa"`
			} `json:"open_access"`
			Locations []struct {
				PDF string `json:"pdf_url"`
			} `json:"locations"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		metrics.WebToolRequests.WithLabelValues("openalex", "error").Inc()
		return nil, fmt.Errorf("the answer of the catalogue is not the JSON expected: %w", err)
	}
	metrics.WebToolRequests.WithLabelValues("openalex", "ok").Inc()
	// Not nil when the catalogue answered: an empty list is a lookup that found nothing.
	works := make([]scholarWork, 0, len(answer.Results))
	for _, w := range answer.Results {
		work := scholarWork{Title: strings.Join(strings.Fields(w.Title), " "), Year: w.Year, Open: w.Access.IsOA}
		for _, a := range w.Authorships {
			if fields := strings.Fields(a.Author.Name); len(fields) > 0 {
				work.Surnames = append(work.Surnames, fields[len(fields)-1])
			}
		}
		seen := map[string]bool{}
		add := func(link string) {
			if link != "" && !seen[link] {
				seen[link] = true
				work.PDFs = append(work.PDFs, link)
			}
		}
		if w.Best != nil {
			add(w.Best.PDF)
		}
		for _, l := range w.Locations {
			add(l.PDF)
		}
		works = append(works, work)
	}
	return works, nil
}

// nameKey is a surname as it is compared: lower case, letters only ("O’Doherty" = "O'Doherty").
func nameKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// authorsAgree tells whether the work has the first `need` surnames of the reference among its authors.
func authorsAgree(w scholarWork, names []string, need int) bool {
	have := map[string]bool{}
	for _, s := range w.Surnames {
		have[nameKey(s)] = true
	}
	if need > len(names) {
		need = len(names)
	}
	for _, n := range names[:need] {
		if !have[nameKey(n)] {
			return false
		}
	}
	return need > 0
}

// workMatches tells whether a work of the catalogue is the reference: with a title, the title as
// matchesReference takes it and the first author among its authors (when the reference names one);
// without one, its first surnames (up to four) all among the authors and the year the same (one year
// either way: a working paper and its article).
func workMatches(w scholarWork, ref reference) bool {
	if w.Title == "" {
		return false
	}
	names := surnames(ref)
	if ref.Title != "" {
		return matchesReference(webHit{Title: w.Title}, ref) && (len(names) == 0 || authorsAgree(w, names, 1))
	}
	year, _ := strconv.Atoi(ref.Year)
	return len(names) >= 2 && authorsAgree(w, names, 4) && (year == 0 || w.Year == 0 || abs(w.Year-year) <= 1)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// scholarFind is what the catalogue knows of a reference.
type scholarFind struct {
	// Title is the title of the first work that is the reference; "" when none is.
	Title string
	// PDFs are the free PDFs of the works that are it (the same work may be there more than once: a
	// working paper, the article) that may be fetched, the best first, at most maxScholarPDFs.
	PDFs []string
	// Open: one of the works that are it is free somewhere, even where the code cannot take it.
	Open bool
}

// maxScholarPDFs is how many copies of a work a step keeps: the first and the ones to try after it.
const maxScholarPDFs = 4

// scholarPDFs is what the catalogue knows of the reference. A PDF is taken when its address is public
// and not of a site that refuses programs or gives only an abstract (Wiley answered 403 to its own
// "pdfdirect" link; SSRN refuses programs), and not a DOI (it leads to the publisher's page).
func scholarPDFs(works []scholarWork, ref reference) scholarFind {
	var f scholarFind
	seen := map[string]bool{}
	for _, w := range works {
		if !workMatches(w, ref) {
			continue
		}
		if f.Title == "" {
			f.Title = w.Title
		}
		f.Open = f.Open || w.Open || len(w.PDFs) > 0
		for _, l := range w.PDFs {
			if ok := scholarLink(l); ok != "" && !seen[urlKey(ok)] && len(f.PDFs) < maxScholarPDFs {
				seen[urlKey(ok)] = true
				f.PDFs = append(f.PDFs, ok)
			}
		}
	}
	return f
}

// scholarLink is a PDF address of the catalogue as it is taken: "" for one that cannot be fetched or
// is no direct PDF (a DOI, which leads to the publisher's page).
func scholarLink(link string) string {
	if _, err := validateFetchURL(link); err != nil {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if closedHost(host) || host == "doi.org" || host == "dx.doi.org" {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}
