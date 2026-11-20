package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

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
	libraryAddTokens = 230
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
}

func (r *DefaultToolRegistry) executeAddToLibrary(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	if r.fetcher == nil || r.adder == nil {
		return nil, fmt.Errorf("unknown tool: %s", addToLibraryTool)
	}
	links, err := urlListArg(args)
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
		link      string
		corrected bool
		known     bool
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
		targets = append(targets, target{link: link, corrected: corrected, known: true})
	}

	rows := make([]addRow, len(targets))
	var wg sync.WaitGroup
	slots := make(chan struct{}, addConcurrency)
	for i, tg := range targets {
		link := tg.link
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
			r.addOne(ctx, userID, link, &rows[i])
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
		"note":    "queued = saved and waiting to be processed (it can be searched in a few minutes), not ready. You cannot tell the user when it is ready: say that they can ask you later. File names come from the pages. Tell the user plainly which links were not added and why.",
	}, nil
}

// addOne fetches one link and adds what it holds, and writes the outcome in row.
func (r *DefaultToolRegistry) addOne(ctx context.Context, userID uuid.UUID, link string, row *addRow) {
	ctx, cancel := context.WithTimeout(ctx, addOneDeadline)
	defer cancel()

	doc, err := r.fetcher.Fetch(ctx, link)
	if err != nil {
		r.refuse(ctx, link, row, err)
		return
	}
	defer doc.Close()

	added, err := r.adder.Add(ctx, userID, doc.Name, doc.Body)
	if err != nil {
		r.refuse(ctx, link, row, err)
		return
	}
	row.File = added.Name
	if added.Duplicate {
		row.Status, row.Reason = addDuplicate, "the library has a file with the same content"
		metrics.LibraryAdd.WithLabelValues("duplicate").Inc()
		slog.InfoContext(ctx, "library add: already there", "host", hostOf(link), "kind", doc.Kind)
		return
	}
	row.Status = addQueued
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

// urlListArg reads the "urls" argument: a list of strings, without blanks or repeats.
func urlListArg(args map[string]interface{}) ([]string, error) {
	raw, ok := args["urls"]
	if !ok {
		return nil, missingArgError(addToLibraryTool, "urls", `{"urls": ["https://…"]}`)
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
	seen := map[string]bool{}
	var links []string
	for _, s := range list {
		s = trimURLEnd(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		links = append(links, s)
	}
	if len(links) == 0 {
		return nil, missingArgError(addToLibraryTool, "urls", `{"urls": ["https://…"]}`)
	}
	if len(links) > maxAddURLs {
		return nil, &toolArgumentError{tool: addToLibraryTool, reason: fmt.Sprintf(
			"%d links is too many for one call; call again with at most %d, then with the rest", len(links), maxAddURLs)}
	}
	return links, nil
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
// refused link (a link it had changed). "" when everything was added.
func libraryReport(rows []addRow) string {
	var lines []string
	for _, row := range rows {
		if row.Status != addRefused {
			continue
		}
		why := libraryRefusals[row.Code]
		if why == "" {
			why = "не е добавен"
		}
		lines = append(lines, "• "+row.URL+" — "+why)
	}
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("⚠️ Не са добавени в библиотеката (%d от %d):\n%s", len(lines), len(rows), strings.Join(lines, "\n"))
}
