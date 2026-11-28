package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/db"
	"go-monolith/internal/ingest"
	"go-monolith/internal/webfetch"
)

// "За всеки линк ми кажи какво е и колко е голям" is answered by the code (2026-11-28). In the Flet
// chat of 2026-11-20 the owner asked "може първо за всеки линк да ми обясниш дали е цяла книга или
// статия и колко място ще заеме" about the sources of a document; the model went through the first
// link by hand and guessed "около 5-10 страници" without opening it. The code opens every link the
// way add_to_library would (the same fetcher, the Wayback copy as the last road), stores nothing,
// and answers with what each one is: a PDF with its size and pages, an article with its words, a
// page with only an abstract, a site that refuses programs, a document already in the library.

const (
	// maxProbeLinks is how many links one answer checks (the ones of add_to_library).
	maxProbeLinks = maxAddURLs
	// probeDeadline bounds the whole answer; probeOneDeadline one link (a PDF is read to its end
	// to know its size and pages).
	probeDeadline    = 2 * time.Minute
	probeOneDeadline = 75 * time.Second
)

var (
	// linkNoun: the links, addresses or sources of the conversation.
	linkNoun = regexp.MustCompile(`(?i)(?:^|\P{L})(?:линк\p{L}*|адрес\p{L}*|източни[кц]\p{L}*|url\p{L}*|links?|urls?|sources?)(?:\P{L}|$)`)
	// linkKindWords: what a link is (a book, an article, a PDF) and how big it is.
	linkKindWords = regexp.MustCompile(`(?i)(?:какво\s+(?:е|са|представлява\p{L}*)|дали\s+(?:е|са)|книга|статия|статии|pdf|` +
		`колко\s+(?:място|голям\p{L}*|е\s+голям\p{L}*|са\s+голям\p{L}*|страници|mb|мб|мегабайт\p{L}*)|размер\p{L}*|обем\p{L}*|` +
		`what\s+(?:is|are)|how\s+(?:big|large|long)|\bsize\b|how\s+many\s+pages)`)
	// linkWorkWords: a request to add the links or make files of them is a task, not this question.
	linkWorkWords = regexp.MustCompile(`(?i)(?:добав|запиш|запаз|създа|направ\p{L}*\s+(?:\p{L}+\s+){0,3}?файл|файл|\badd\b|\bsave\b|\bcreate\b|\bmake\b|download|свал)`)
)

// asksAboutLinks tells whether a message asks what the links are and how big: "за всеки линк ми
// кажи дали е книга или статия и колко място ще заеме", "какво са тези линкове?". The words of the
// links themselves do not count: "ето източниците: https://a.org/paper.pdf" asks nothing.
func asksAboutLinks(message string) bool {
	message = strings.TrimSpace(urlPattern.ReplaceAllString(withoutClosingNote(message), " "))
	if message == "" || utf8.RuneCountInString(message) > maxMultiStepRunes {
		return false
	}
	return linkNoun.MatchString(message) && linkKindWords.MatchString(message) && !linkWorkWords.MatchString(message)
}

// lastUserLinks are the links of the newest message of the person that has any, in their order,
// once each (by urlKey), at most maxProbeLinks; and whether there were more.
func lastUserLinks(history []openai.ChatCompletionMessage) ([]string, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != openai.ChatMessageRoleUser {
			continue
		}
		var links []string
		seen := map[string]bool{}
		more := false
		for _, u := range extractURLs(history[i].Content) {
			k := urlKey(u)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			if len(links) == maxProbeLinks {
				more = true
				continue
			}
			links = append(links, u)
		}
		if len(links) > 0 {
			return links, more
		}
	}
	return nil, false
}

// linkProbe is what one link is.
type linkProbe struct {
	Link  string
	Title string
	Kind  webfetch.Kind
	Bytes int64
	// Pages of a PDF (0: not known); Words of the text of a page.
	Pages, Words int
	// Archived is the day of the Wayback copy that was read, Because why the site was not.
	Archived time.Time
	Because  string
	// InLibrary is the library file with the same content, InLibraryID its id.
	InLibrary   string
	InLibraryID uuid.UUID
	// Reason is why the link gives nothing ("" when it gives a document).
	Reason string
}

// linkProber checks links without storing them (DefaultToolRegistry).
type linkProber interface {
	canProbeLinks() bool
	probeLinks(ctx context.Context, userID uuid.UUID, links []string) []linkProbe
}

func (r *DefaultToolRegistry) canProbeLinks() bool { return r.fetcher != nil }

// probeLinks checks the links, addConcurrency at a time, in the order given.
func (r *DefaultToolRegistry) probeLinks(ctx context.Context, userID uuid.UUID, links []string) []linkProbe {
	ctx, cancel := context.WithTimeout(ctx, probeDeadline)
	defer cancel()
	out := make([]linkProbe, len(links))
	slots := make(chan struct{}, addConcurrency)
	var wg sync.WaitGroup
	for i, link := range links {
		out[i].Link = link
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				out[i].Reason = string(webfetch.ReasonTimeout)
				return
			}
			defer func() { <-slots }()
			r.probeOne(ctx, userID, &out[i])
		}()
	}
	wg.Wait()
	return out
}

// probeOne opens one link as add_to_library would, reads it to its end and writes what it is in p.
func (r *DefaultToolRegistry) probeOne(ctx context.Context, userID uuid.UUID, p *linkProbe) {
	ctx, cancel := context.WithTimeout(ctx, probeOneDeadline)
	defer cancel()
	// A link already in the library is told from the library, without a download.
	if known, ok := r.librarySource(ctx, userID, p.Link); ok {
		p.Title, p.Kind, p.Bytes = known.FileName, kindOfFile(known.FileName), known.FileSize
		p.InLibrary, p.InLibraryID = known.FileName, known.ID
		return
	}
	doc, err := r.fetcher.Fetch(ctx, p.Link)
	if err != nil {
		copied, because := r.archivedDocument(ctx, p.Link, err)
		if copied == nil {
			p.Reason = probeReason(ctx, err)
			slog.InfoContext(ctx, "link check: nothing", "host", hostOf(p.Link), "reason", p.Reason)
			return
		}
		doc, p.Archived, p.Because = copied, copied.Archived, because
	}
	defer doc.Close()
	p.Kind, p.Title = doc.Kind, strings.TrimSpace(doc.Title)
	if p.Title == "" {
		p.Title = strings.TrimSpace(doc.Name)
	}

	hasher := sha256.New()
	if doc.Kind == webfetch.KindPDF {
		err = r.probePDF(ctx, doc.Body, hasher, p)
	} else {
		err = probePage(ctx, doc.Body, hasher, p)
	}
	if err != nil {
		p.Reason = probeReason(ctx, err)
		slog.InfoContext(ctx, "link check: not read to its end", "host", hostOf(p.Link), "reason", p.Reason)
		return
	}
	if r.db != nil {
		file, err := r.db.Queries.GetIngestedFileByHash(ctx, db.GetIngestedFileByHashParams{UserID: userID, FileHash: hex.EncodeToString(hasher.Sum(nil))})
		if err == nil {
			p.InLibrary, p.InLibraryID = file.FileName, file.ID
		}
	}
	slog.InfoContext(ctx, "link check: done", "host", hostOf(p.Link), "kind", p.Kind, "bytes", p.Bytes, "pages", p.Pages)
}

// probePDF reads a PDF into a temporary file (pdfinfo needs one) and counts its pages.
func (r *DefaultToolRegistry) probePDF(ctx context.Context, body io.Reader, hasher hash.Hash, p *linkProbe) error {
	tmp, err := os.CreateTemp("", "link-check-*.pdf")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	p.Bytes = n
	if pages, known, err := ingest.NewPDFExtractor().PageCount(ctx, tmp.Name()); err == nil && known {
		p.Pages = pages
	}
	return nil
}

// probePage reads a page (the fetcher bounds it) and counts the words of its text.
func probePage(ctx context.Context, body io.Reader, hasher hash.Hash, p *linkProbe) error {
	raw, err := io.ReadAll(io.TeeReader(body, hasher))
	if err != nil {
		return err
	}
	p.Bytes = int64(len(raw))
	if text, err := ingest.NewHTMLExtractor().Extract(ctx, bytes.NewReader(raw)); err == nil {
		p.Words = len(strings.Fields(text))
	}
	return nil
}

// probeReason is the reason of a failed link, as webfetch names it.
func probeReason(ctx context.Context, err error) string {
	var fe *webfetch.Error
	switch {
	case errors.As(err, &fe):
		return string(fe.Reason)
	case errors.Is(err, webfetch.ErrTooLarge):
		return string(webfetch.ReasonTooLarge)
	case errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		return string(webfetch.ReasonTimeout)
	}
	return string(webfetch.ReasonUnreachable)
}

// probeReasonWords says a reason in the person's words.
var probeReasonWords = map[string]string{
	string(webfetch.ReasonBlocked):      "сайтът не пуска програми (отказ 401/403)",
	string(webfetch.ReasonRateLimited):  "сайтът иска да изчакаме (429) — опитай по-късно",
	string(webfetch.ReasonNotFound):     "няма го на този адрес (404)",
	string(webfetch.ReasonUnreachable):  "сайтът не отговаря",
	string(webfetch.ReasonTimeout):      "сайтът не отговори навреме",
	string(webfetch.ReasonUnsupported):  "не е нито PDF, нито уеб страница с текст",
	string(webfetch.ReasonAbstractOnly): "страницата има само резюме — пълният текст не е там (често е платен)",
	string(webfetch.ReasonNotPublic):    "адресът не е публичен",
	string(webfetch.ReasonBadURL):       "адресът е сбъркан",
}

// archivedReasonWords: why the site itself gave nothing, when its Wayback copy was read.
var archivedReasonWords = map[string]string{
	string(webfetch.ReasonBlocked):     "не пуска програми",
	string(webfetch.ReasonRateLimited): "иска да изчакаме",
	string(webfetch.ReasonNotFound):    "вече го няма",
	string(webfetch.ReasonUnreachable): "не отговаря",
	string(webfetch.ReasonTimeout):     "не отговори навреме",
}

// noLinksText answers the question when the person wrote no link in the conversation.
const noLinksText = "Не виждам линкове, написани в този разговор (линкове в прикачен файл не чета за това). " +
	"Постави ги в съобщение — може и много наведнъж — и ще проверя всеки: дали е PDF или статия, колко е голям и дали сайтът го дава."

// linkProbeAnswer is the answer of the code to "какво са тези линкове и колко са големи?", "" when
// the model is to answer.
func (o *Orchestrator) linkProbeAnswer(ctx context.Context, userID uuid.UUID, message string, history []openai.ChatCompletionMessage) string {
	if !o.addToLibrary || !asksAboutLinks(message) {
		return ""
	}
	prober, ok := o.tools.(linkProber)
	if !ok || !prober.canProbeLinks() {
		return ""
	}
	links, more := lastUserLinks(history)
	if len(links) == 0 {
		return noLinksText
	}
	// The fetcher takes at most webfetch.DefaultMaxBytes (main.go), the size too_large names.
	return linkProbeText(prober.probeLinks(ctx, userID, links), more, webfetch.DefaultMaxBytes)
}

// linkProbeText is the answer: a line for every link, then what can be added.
func linkProbeText(probes []linkProbe, more bool, maxBytes int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Проверих линковете (%d) — само ги отворих, нищо не е записано:\n", len(probes))
	usable, inLibrary := 0, 0
	var total int64
	for i, p := range probes {
		name := p.Title
		if name == "" {
			name = hostOf(p.Link)
		}
		fmt.Fprintf(&b, "\n%d. %s — %s\n   %s\n", i+1, truncateRunes(name, 90), probeLine(p, maxBytes), shortURL(p.Link))
		switch {
		case p.Reason != "":
		case p.InLibrary != "":
			inLibrary++
		default:
			usable++
			total += p.Bytes
		}
	}
	if more {
		fmt.Fprintf(&b, "\nПроверих първите %d; за останалите ги прати в отделно съобщение.\n", maxProbeLinks)
	}
	b.WriteString("\n")
	switch {
	case usable > 0:
		fmt.Fprintf(&b, "Могат да се добавят в библиотеката: %d (общо ≈ %s).", usable, sizeWords(total))
		if inLibrary > 0 {
			fmt.Fprintf(&b, " Вече са там: %d.", inLibrary)
		}
		b.WriteString(" Ако искаш, кажи „добави ги в библиотеката“.")
	case inLibrary > 0:
		b.WriteString("Нищо ново за добавяне: тези, които се отварят, вече са в библиотеката.")
	default:
		b.WriteString("Нито един не дава документ, който мога да добавя.")
	}
	return b.String()
}

// probeLine is what one link is, in the person's words.
func probeLine(p linkProbe, maxBytes int64) string {
	if p.Reason != "" {
		if p.Reason == string(webfetch.ReasonTooLarge) {
			return fmt.Sprintf("по-голям от %s — не мога да го сваля", sizeWords(maxBytes))
		}
		if words, ok := probeReasonWords[p.Reason]; ok {
			return words
		}
		return "не се отваря"
	}
	var line string
	if p.Kind == webfetch.KindPDF {
		line = "PDF, " + sizeWords(p.Bytes)
		if p.Pages > 0 {
			line += fmt.Sprintf(", %d %s", p.Pages, plural(p.Pages, "страница", "страници"))
		}
		if p.Pages >= 150 {
			line += " — по-скоро книга"
		}
	} else {
		line = "статия (уеб страница)"
		if p.Words > 0 {
			line += fmt.Sprintf(", ≈ %s думи", thousands(roundWords(p.Words)))
		}
	}
	if !p.Archived.IsZero() {
		why := archivedReasonWords[p.Because]
		if why == "" {
			why = "не го дава"
		}
		line += fmt.Sprintf("; сайтът %s, затова прочетох копието от %s в Wayback Machine", why, p.Archived.Format("02.01.2006"))
	}
	if p.InLibrary != "" {
		line += "; вече е в библиотеката като „" + p.InLibrary + "“"
	}
	return line
}

// sizeWords is a size the way a person reads it: "850 KB", "2,3 MB".
func sizeWords(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", max(1, (n+512)>>10))
	}
	return strings.Replace(fmt.Sprintf("%.1f MB", float64(n)/(1<<20)), ".", ",", 1)
}

// roundWords: a count of a page's words is rough (the menus count too), so it is said in hundreds.
func roundWords(n int) int {
	if n < 1000 {
		return n
	}
	return (n + 50) / 100 * 100
}

// thousands writes 6200 as "6 200".
func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
