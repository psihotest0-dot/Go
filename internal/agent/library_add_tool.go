package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// addToLibraryTool is the tool with which the assistant downloads the documents behind
// the links a person wrote and puts them in the library (plan item P53). It is offered only
// in a conversation where the person wrote a link (see offersLibraryAdd).
//
// The model never gets the text: the tool answers with a table of what was added. A text
// from the internet that the model read could ask for anything; a table the server made
// cannot (the names of the files come from the pages, so the turn counts as one that read
// the web - see executeAddToLibrary).
const addToLibraryTool = "add_to_library"

const (
	// maxAddURLs is how many links one call takes; a longer list is made in two calls.
	maxAddURLs = 25
	// addConcurrency is how many downloads run at once: a polite number for the sites.
	addConcurrency = 4
	// addDeadline bounds a whole call (a turn of Telegram has 3 minutes).
	addDeadline = 2 * time.Minute
	// addOneDeadline bounds one link.
	addOneDeadline = 75 * time.Second
	// libraryAddTokens is what the definition of the tool costs in a request, taken out of
	// the room of the history in a turn that offers it (the budgets of a turn without a
	// link are not changed).
	// 230 measured; the name and the names of 2026-11-23 add 61 (185 -> 246 tokens of the JSON).
	libraryAddTokens = 295
	// maxReasonRunes bounds a reason shown to the model.
	maxReasonRunes = 220
)

// Statuses of a link in the table the model gets.
const (
	addQueued    = "queued"
	addDuplicate = "already_in_library"
	addRefused   = "not_added"
)

func addToLibraryDefinition() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name: addToLibraryTool,
			Description: "Download the documents behind links the user wrote in this conversation (PDF files and web articles) and add them to the user's library. " +
				"Use it when the user asks to download, fetch, save or add the sources, papers or links to the library. " +
				"You get no text, only a table: 'queued' means saved and waiting to be processed, not ready to search yet. " +
				"Only links the user wrote can be used. A page that holds only an abstract is not added.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"urls": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": fmt.Sprintf("The links to add, exactly as the user wrote them (at most %d at a time).", maxAddURLs),
					},
					"name": map[string]any{
						"type":        "string",
						"description": "For one link: the title the user gave the document, as they wrote it. Leave it out if the user gave no title.",
					},
					"names": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "For several links: the titles the user gave, in the order of urls (\"\" for a link without one).",
					},
				},
				"required": []string{"urls"},
			},
		},
	}
}

// documentFetcher downloads the document behind a link (webfetch.Fetcher).
type documentFetcher interface {
	Fetch(ctx context.Context, rawURL string) (*webfetch.Document, error)
}

// libraryAdder puts a document into the library of a user (api.LibraryAdder).
type libraryAdder interface {
	Add(ctx context.Context, userID uuid.UUID, name string, src io.Reader) (api.AddedFile, error)
}

// NewLibraryFetcher is the fetcher of add_to_library: the limit is the largest document, and
// the names that are not public ones are refused before any request, with the same rules as
// web_fetch (validateFetchURL).
func NewLibraryFetcher(maxBytes int64) *webfetch.Fetcher {
	return webfetch.New(webfetch.Options{
		MaxBytes: maxBytes,
		CheckURL: func(raw string) error {
			_, err := validateFetchURL(raw)
			return err
		},
	})
}

// WithLibraryAdd lets the tool add_to_library work.
func (r *DefaultToolRegistry) WithLibraryAdd(f documentFetcher, a libraryAdder) *DefaultToolRegistry {
	r.fetcher, r.adder = f, a
	return r
}

var (
	libraryWord    = regexp.MustCompile(`(?i)(библиотек|\blibrary\b)`)
	libraryAddVerb = regexp.MustCompile(`(?i)(добав|свал|качи|качва|качете|сложи|постав|запиш|запаз|прибав|\badd\b|\bput\b|\bsave\b|download|import|upload)`)
)

// asksToAddToLibrary tells whether a message asks for the document behind a link it holds to go
// into the library. Left to itself the model called add_to_library for the first such message of
// a conversation, and answered "Добавих и този линк" without a call for the next one - 2 of 2 in
// the live check of 2026-11-24 (go-monolith-backups\measurements\live_2026-11-24), with no
// warning under it, since the warning comes from the tool; 6 of 6 in fresh conversations. Such a
// message has a link, the library and a word for adding.
func asksToAddToLibrary(message string) bool {
	if !pageLink.MatchString(message) {
		return false
	}
	text := pageLink.ReplaceAllString(message, " ")
	return libraryWord.MatchString(text) && libraryAddVerb.MatchString(text)
}

// offersLibraryAdd tells whether a turn offers add_to_library: the tool is on, and the
// person wrote a link in the conversation.
func (o *Orchestrator) offersLibraryAdd(urls map[string]string) bool {
	return o.addToLibrary && len(urls) > 0
}

// addRow is a line of the table the model gets.
type addRow struct {
	URL    string `json:"url"`
	Status string `json:"status"`
	File   string `json:"file,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Code is why the link was not added, in a word (libraryRefusals): the model gets the
	// Reason, the person gets the line made from the Code (libraryReport).
	Code string `json:"-"`
	// Note says that the link was not the one the model sent but the one the user wrote.
	Note string `json:"note,omitempty"`
	// Archived says that the document is the copy the Wayback Machine kept, and why (wayback.go);
	// ArchivedOn is its day, for the line of the person (libraryReport).
	Archived   string    `json:"archived_copy,omitempty"`
	ArchivedOn time.Time `json:"-"`
}

func (r *DefaultToolRegistry) executeAddToLibrary(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	if r.fetcher == nil || r.adder == nil {
		return nil, fmt.Errorf("unknown tool: %s", addToLibraryTool)
	}
	links, names, err := urlListArg(args)
	if err != nil {
		return nil, err
	}
	turn := turnFrom(ctx)

	ctx, cancel := context.WithTimeout(ctx, addDeadline)
	defer cancel()

	// The person must have written the link: an address the model made up, or took from a
	// page, is a way to send out what it has read. A link that differs from one the person
	// wrote by a few characters is that link, copied wrongly (resolveURL); two spellings of
	// one link are fetched once.
	type target struct {
		link, name string
		corrected  bool
		known      bool
	}
	targets := make([]target, 0, len(links))
	seen := map[string]bool{}
	for _, sent := range links {
		link, corrected, ok := turn.resolveURL(sent)
		if !ok {
			targets = append(targets, target{link: sent})
			continue
		}
		if seen[link] {
			continue
		}
		seen[link] = true
		targets = append(targets, target{link: link, name: names[sent], corrected: corrected, known: true})
	}

	rows := make([]addRow, len(targets))
	var wg sync.WaitGroup
	slots := make(chan struct{}, addConcurrency)
	for i, tg := range targets {
		link, name := tg.link, tg.name
		rows[i] = addRow{URL: shortURL(link)}
		if tg.corrected {
			rows[i].Note = "the link differed from the one the user wrote in a few characters; the link of the user was used"
			metrics.LibraryAdd.WithLabelValues("corrected").Inc()
		}
		if !tg.known {
			rows[i].Status, rows[i].Code, rows[i].Reason = addRefused, "not_written", "the user did not write this link in this conversation"
			metrics.LibraryAdd.WithLabelValues("not_written").Inc()
			continue
		}
		if _, err := validateFetchURL(link); err != nil {
			rows[i].Status, rows[i].Code, rows[i].Reason = addRefused, "not_public", truncateRunes(err.Error(), maxReasonRunes)
			metrics.LibraryAdd.WithLabelValues("not_public").Inc()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Wait for a place; the time of the whole call may run out first.
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				rows[i].Status, rows[i].Code, rows[i].Reason = addRefused, "not_tried", "not tried: the time of the call ran out; call again with the rest"
				metrics.LibraryAdd.WithLabelValues("timeout").Inc()
				return
			}
			r.addOne(ctx, userID, link, name, &rows[i])
		}()
	}
	wg.Wait()

	// The names of the files come from the pages: the model has read text from the web.
	turn.markExternal(sourceWebFetch)
	// What was not added is told to the person under the answer, whatever the model says of it.
	turn.addLibraryRows(rows)

	queued, duplicates := 0, 0
	for _, row := range rows {
		switch row.Status {
		case addQueued:
			queued++
		case addDuplicate:
			duplicates++
		}
	}
	return map[string]interface{}{
		"results": rows,
		"summary": fmt.Sprintf("%d of %d queued, %d already in the library, %d not added", queued, len(rows), duplicates, len(rows)-queued-duplicates),
		"note":    "queued = saved and waiting to be processed (it can be searched in a few minutes), not ready. You cannot tell the user when it is ready: say that they can ask you later. A file is named as the user named it, else after its page; tell the user the name it got. Tell the user plainly which links were not added and why.",
	}, nil
}

// addOne fetches one link and adds what it holds, and writes the outcome in row. name is the title
// the user gave the document, "" for the name the page gives it.
func (r *DefaultToolRegistry) addOne(ctx context.Context, userID uuid.UUID, link, name string, row *addRow) {
	ctx, cancel := context.WithTimeout(ctx, addOneDeadline)
	defer cancel()

	// A link already in the library is not fetched again (library_source.go).
	if known, ok := r.librarySource(ctx, userID, link); ok {
		row.Status, row.Reason, row.File = addDuplicate, "the library has the document of this link already", known.FileName
		metrics.LibraryAdd.WithLabelValues("duplicate").Inc()
		slog.InfoContext(ctx, "library add: the link is in the library", "host", hostOf(link))
		return
	}

	doc, err := r.fetcher.Fetch(ctx, link)
	if err != nil {
		// The last road: the copy in the Wayback Machine (wayback.go).
		copied, because := r.archivedDocument(ctx, link, err)
		if copied == nil {
			r.refuse(ctx, link, row, err)
			return
		}
		doc = copied
		row.Code, row.ArchivedOn = because, copied.Archived
		row.Archived = archivedNote(copied.Archived, because)
	}
	defer doc.Close()

	added, err := r.adder.Add(ctx, userID, libraryFileName(name, doc.Name), doc.Body)
	if err != nil {
		r.refuse(ctx, link, row, err)
		return
	}
	row.File = added.Name
	r.rememberSource(ctx, userID, added.ID, link)
	if added.Duplicate {
		row.Status, row.Reason = addDuplicate, "the library has a file with the same content"
		metrics.LibraryAdd.WithLabelValues("duplicate").Inc()
		slog.InfoContext(ctx, "library add: already there", "host", hostOf(link), "kind", doc.Kind)
		return
	}
	row.Status = addQueued
	if !row.ArchivedOn.IsZero() {
		metrics.LibraryAdd.WithLabelValues("wayback").Inc()
	}
	metrics.LibraryAdd.WithLabelValues("queued").Inc()
	slog.InfoContext(ctx, "library add: queued", "host", hostOf(link), "kind", doc.Kind, "bytes", added.Size)
}

// refuse writes why a link was not added in row, and counts it.
func (r *DefaultToolRegistry) refuse(ctx context.Context, link string, row *addRow, err error) {
	row.Status = addRefused
	result := "error"
	var fetchErr *webfetch.Error
	switch {
	case errors.As(err, &fetchErr):
		result = string(fetchErr.Reason)
		row.Reason = truncateRunes(fetchErr.Error(), maxReasonRunes)
	case errors.Is(err, webfetch.ErrTooLarge), errors.Is(err, api.ErrAddTooLarge):
		result = string(webfetch.ReasonTooLarge)
		row.Reason = "too_large: the document is larger than the limit"
	case errors.Is(err, api.ErrAddEmpty):
		result = "empty"
		row.Reason = "the document was empty"
	case errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		result = string(webfetch.ReasonTimeout)
		row.Reason = "timeout: the site did not finish in time"
	default:
		// A failure of this server, not of the link: logged in full, not shown.
		slog.ErrorContext(ctx, "library add failed", "host", hostOf(link), "error", err)
		row.Reason = "the server could not store the document"
	}
	row.Code = result
	metrics.LibraryAdd.WithLabelValues(result).Inc()
	slog.InfoContext(ctx, "library add: not added", "host", hostOf(link), "result", result)
}

// urlListArg reads the "urls" argument: a list of strings, without blanks or repeats; and the titles
// the user gave ("names", in the order of urls, or "name" for one link), by link.
func urlListArg(args map[string]interface{}) ([]string, map[string]string, error) {
	raw, ok := args["urls"]
	if !ok {
		return nil, nil, missingArgError(addToLibraryTool, "urls", `{"urls": ["https://…"]}`)
	}
	var list []string
	switch v := raw.(type) {
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				list = append(list, s)
			}
		}
	case []string:
		list = v
	case string:
		// A model that sends one link as a string.
		list = []string{v}
	}
	given := stringListArg(args["names"])
	if one, ok := stringArg(args, "name"); ok && len(list) == 1 {
		given = []string{one}
	}
	seen := map[string]bool{}
	names := map[string]string{}
	var links []string
	for i, s := range list {
		s = trimURLEnd(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		links = append(links, s)
		if i < len(given) && strings.TrimSpace(given[i]) != "" {
			names[s] = strings.TrimSpace(given[i])
		}
	}
	if len(links) == 0 {
		return nil, nil, missingArgError(addToLibraryTool, "urls", `{"urls": ["https://…"]}`)
	}
	if len(links) > maxAddURLs {
		return nil, nil, &toolArgumentError{tool: addToLibraryTool, reason: fmt.Sprintf(
			"%d links is too many for one call; call again with at most %d, then with the rest", len(links), maxAddURLs)}
	}
	return links, names, nil
}

// stringListArg is a list of strings from a JSON array (other items left out), or one string.
func stringListArg(raw interface{}) []string {
	switch v := raw.(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, _ := item.(string)
			out = append(out, s)
		}
		return out
	case []string:
		return v
	case string:
		return []string{v}
	}
	return nil
}

// maxLibraryNameRunes bounds the name a title gives a file of the library.
const maxLibraryNameRunes = 120

// libraryFileName is the name of a document of the library: the title the user gave it, with the
// extension of the document (a PDF stays a PDF whatever the title says), or the name the page
// gave it when there is no title, or nothing is left of it. The title keeps letters, digits and
// the marks of a title; a path, a control character or a mark of a shell does not come through.
// (The owner, 2026-11-23: a book added "под заглавие …" was named after the last part of its
// link, an abbreviation.)
func libraryFileName(title, docName string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return docName
	}
	ext := path.Ext(docName)
	if strings.EqualFold(path.Ext(title), ext) {
		title = strings.TrimSpace(title[:len(title)-len(ext)])
	}
	var b strings.Builder
	space := false
	for _, r := range title {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.,()'&!+", r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		default:
			space = true
		}
	}
	stem := strings.Trim(b.String(), " .")
	if utf8.RuneCountInString(stem) > maxLibraryNameRunes {
		stem = strings.TrimRight(string([]rune(stem)[:maxLibraryNameRunes]), " .")
	}
	if stem == "" {
		return docName
	}
	return stem + ext
}

// shortURL is a link as the table shows it: long ones are cut.
func shortURL(link string) string { return truncateRunes(link, 160) }

// hostOf is the host of a link, for the log: the path and the query are the person's.
func hostOf(link string) string {
	if u, err := url.Parse(link); err == nil {
		return u.Hostname()
	}
	return ""
}

// libraryRefusals says, in the language of the person, why a link was not added. The code is
// the Reason of webfetch or one of the checks of the tool.
var libraryRefusals = map[string]string{
	"not_written":   "този линк не е написан от теб в разговора (променен ли е?)",
	"not_public":    "адресът е на тази машина или на частна мрежа",
	"not_tried":     "не е опитан — времето за извикването свърши",
	"blocked":       "сайтът не пуска програми (401/403)",
	"rate_limited":  "сайтът ограничава заявките (429) — опитай по-късно",
	"not_found":     "не е намерен (404)",
	"unreachable":   "няма връзка със сайта",
	"timeout":       "сайтът не отговори навреме",
	"too_large":     "файлът е по-голям от позволеното",
	"unsupported":   "не е PDF и не е уеб страница",
	"abstract_only": "страницата е само резюме, а не цялата статия",
	"bad_url":       "не е http или https линк",
	"empty":         "документът е празен",
	"error":         "грешка на сървъра",
}

// libraryReport is the line the person reads under the answer when some links were not
// added: the model was told, but in the real runs it wrote "всички са добавени" over one
// refused link (a link it had changed); and the links taken from their copy in the Wayback Machine,
// with its day. "" when every link was added from its site.
func libraryReport(rows []addRow) string {
	var lines, copies []string
	for _, row := range rows {
		if row.Status != addRefused {
			if !row.ArchivedOn.IsZero() {
				copies = append(copies, "• "+row.URL+" — копие от "+row.ArchivedOn.Format("02.01.2006")+" (самата страница: "+libraryRefusals[row.Code]+")")
			}
			continue
		}
		why := libraryRefusals[row.Code]
		if why == "" {
			why = "не е добавен"
		}
		lines = append(lines, "• "+row.URL+" — "+why)
	}
	var parts []string
	if len(lines) > 0 {
		parts = append(parts, fmt.Sprintf("⚠️ Не са добавени в библиотеката (%d от %d):\n%s", len(lines), len(rows), strings.Join(lines, "\n")))
	}
	if len(copies) > 0 {
		parts = append(parts, "ℹ️ Взети от архива web.archive.org, защото самата страница не се отвори:\n"+strings.Join(copies, "\n"))
	}
	return strings.Join(parts, "\n\n")
}
