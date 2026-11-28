package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// The sources of a list of references are found on the internet by the code (plan П55, phase 3,
// recipe Р3; 2026-11-28). In the Flet chat of 2026-11-21 the owner asked "опитай от раздел 11 да
// потърсиш в интернет дали някой от източниците може да се извлече" about the references of a review
// he had attached: some had no link at all ("Sullivan, Timmermann, White (1999)"), and of the 21
// links six gave only an abstract and one refused programs (the link check of 2026-11-28). The model
// went on for ~45 turns, one source at a time, with promises and "успя ли?" four times.
//
// Now such a message is answered by the code: the references are read from the person's message (or
// a recent one), or from the section the message names of a file attached to the conversation; the
// person's own links are checked as "за всеки линк ми кажи какво е" checks them; for each reference
// without a link that gives its text, the search looks for a free full text (a PDF, a preprint).
// The proposal lists what was found and what was not, and the person's yes starts the task over the
// links (link_task.go): each is put into the library and its file is made. The addresses the search
// found are shown in the proposal and none is opened before the yes - the rule the owner approved on
// 2026-11-28: the yes on the shown list makes them the person's.

var (
	// sourcesNounPlural: the sources, references or citations, as many ("източниците", "справките");
	// one source ("започни търсенето на първият източник") is not a list.
	sourcesNounPlural = regexp.MustCompile(`(?i)(?:^|\P{L})(?:източни[цж]\p{L}*|справки|справките|литература\p{L}*|цитира\p{L}*|библиограф\p{L}*|references|sources|bibliograph\p{L}*|citations)(?:\P{L}|$)`)
	// sourcesFindVerb: they are to be looked for.
	sourcesFindVerb = regexp.MustCompile(`(?i)(?:^|\P{L})(?:потърс\p{L}*|намер\p{L}*|търс\p{L}*|открий\p{L}*|откриеш|достигн\p{L}*|search\p{L}*|find|look\s+up)(?:\P{L}|$)`)
	// sectionNumber: "от раздел 11", "11-източници", "точка 3".
	sectionNumber = regexp.MustCompile(`(?i)(?:(?:раздел|секция|глава|точка|част|section|chapter)\s*(?:№\s*)?(\d{1,2})(?:\D|$)|(?:^|\D)(\d{1,2})\s*[-–.]?\s*(?:ти|ви|ри|ми)?\s*(?:раздел|източни|справк))`)
	// referenceFileWords: the message is about a file attached to the conversation.
	referenceFileWords = regexp.MustCompile(`(?i)(?:файл|прикач|закач|закреп|ревю|документ\p{L}*|attach|\bfile\b)`)
)

// asksToFindSources tells whether a message asks to look for the sources of a list: "опитай от раздел
// 11 да потърсиш в интернет дали някой от източниците може да се извлече".
func asksToFindSources(message string) bool {
	m := strings.TrimSpace(urlPattern.ReplaceAllString(withoutClosingNote(message), " "))
	if m == "" || utf8.RuneCountInString(m) > maxKnowledgeQuestionRunes {
		return false
	}
	return sourcesNounPlural.MatchString(m) && sourcesFindVerb.MatchString(m)
}

// reference is one entry of a list of sources.
type reference struct {
	// Text is the entry without the markup of its link; Title its title, when it has one.
	Text, Title string
	// Link is the address the person wrote for it; Year the year of the entry.
	Link, Year string
}

const (
	// maxReferences is how many references one proposal takes (the steps of a plan).
	maxReferences = taskplan.MaxSteps
	// maxReferenceRunes: a longer line is prose, not an entry of a list.
	maxReferenceRunes = 600
	// recentReferenceMessages is how far back the person's messages are looked at for a list.
	recentReferenceMessages = 8
	// sourceSearchDeadline bounds all the searches of a proposal.
	sourceSearchDeadline = 90 * time.Second
)

var (
	// markdownLink is "[title](address)"; the address may hold a pair of brackets.
	markdownLink = regexp.MustCompile(`\[([^\]]{2,300})\]\((https?://[^\s()]+(?:\([^\s()]*\)[^\s()]*)*)\)`)
	// entryYear is a year of a reference: (1999), (JFE 2012), ", 2006.".
	entryYear = regexp.MustCompile(`(?:\(|,\s*)(?:[\p{L}&.\s]{0,30}\s)?((?:19|20)\d\d)[a-z]?(?:\)|\.|,|$)`)
	// listBullet is the mark of an entry of a list: "- ", "• ", "1. ", "[3] ".
	listBullet = regexp.MustCompile(`^\s*(?:[-*•–]|\d{1,3}[.)]|\[\d{1,3}\])\s+`)
	// quotedTitle is a title in quotes.
	quotedTitle = regexp.MustCompile(`[„"“«]([^"“”«»„]{8,200})[“”"»]`)
	// surnameList is an entry that is only names: "Ang, Hodrick, Xing, Zhang".
	surnameList = regexp.MustCompile(`^\p{Lu}[\p{L}'’-]+(?:,\s*(?:and\s+|&\s*)?\p{Lu}[\p{L}'’-]+){1,8}\.?$`)
)

// parseReferences are the entries of a list of sources in text, in their order, once each.
func parseReferences(text string) []reference {
	var out []reference
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(listBullet.ReplaceAllString(line, ""))
		if n := utf8.RuneCountInString(line); n < 6 || n > maxReferenceRunes {
			continue
		}
		for _, item := range splitEntries(line) {
			ref, ok := parseEntry(item)
			if !ok {
				continue
			}
			key := strings.ToLower(ref.Title + "|" + ref.Link + "|" + ref.Text)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, ref)
		}
	}
	return out
}

// splitEntries cuts a line at the semicolons that are outside brackets: one line may hold several
// references ("…, [Technical trading revisited](…) (JFE 2012); Sullivan, Timmermann, White (1999)").
func splitEntries(line string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range line {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				out = append(out, strings.TrimSpace(line[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(line[start:]))
}

// parseEntry reads one reference; ok is false for text that is no reference.
func parseEntry(item string) (reference, bool) {
	item = strings.TrimSpace(strings.TrimRight(item, " ."))
	if item == "" {
		return reference{}, false
	}
	var ref reference
	if m := markdownLink.FindStringSubmatchIndex(item); m != nil {
		ref.Title = strings.TrimSpace(item[m[2]:m[3]])
		ref.Link = trimURLEnd(item[m[4]:m[5]])
		item = item[:m[0]] + ref.Title + item[m[1]:]
	} else if links := extractURLs(item); len(links) > 0 {
		ref.Link = links[0]
		item = strings.TrimSpace(urlPattern.ReplaceAllString(item, " "))
	}
	if ref.Title == "" {
		if m := quotedTitle.FindStringSubmatch(item); m != nil {
			ref.Title = strings.TrimSpace(m[1])
		}
	}
	if ys := entryYear.FindAllStringSubmatch(item, -1); len(ys) > 0 {
		ref.Year = ys[len(ys)-1][1]
	}
	ref.Text = strings.Join(strings.Fields(item), " ")
	switch {
	case ref.Link != "" || ref.Title != "":
		return ref, hasCapital(ref.Text)
	case ref.Year != "":
		// Text only: a reference has names before its year ("Sullivan, Timmermann, White (1999)").
		return ref, hasCapital(authorsOf(ref))
	}
	return ref, surnameList.MatchString(ref.Text)
}

func hasCapital(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// authorsOf is the part of a reference before its title or year: its authors, as written.
func authorsOf(ref reference) string {
	text := ref.Text
	cut := len(text)
	if ref.Title != "" {
		if i := strings.Index(text, ref.Title); i >= 0 && i < cut {
			cut = i
		}
	}
	if ref.Year != "" {
		if i := strings.Index(text, ref.Year); i >= 0 && i < cut {
			cut = i
		}
	}
	return strings.Trim(text[:cut], " ,.:;(\"„“")
}

// surnames are the capitalised words of the authors of a reference, at most four.
func surnames(ref reference) []string {
	var out []string
	for _, w := range strings.FieldsFunc(authorsOf(ref), func(r rune) bool { return r == ',' || r == ' ' || r == '&' }) {
		w = strings.Trim(w, ".")
		if utf8.RuneCountInString(w) < 2 || !unicode.IsUpper([]rune(w)[0]) || w == "And" {
			continue
		}
		out = append(out, w)
		if len(out) == 4 {
			break
		}
	}
	return out
}

// referenceName is how a reference is named in a list: its title with its first author and year,
// else its text.
func referenceName(ref reference) string {
	if ref.Title == "" {
		return truncateRunes(ref.Text, 90)
	}
	name := truncateRunes(ref.Title, 80)
	var tail []string
	if s := surnames(ref); len(s) > 0 {
		tail = append(tail, s[0])
	}
	if ref.Year != "" {
		tail = append(tail, ref.Year)
	}
	if len(tail) > 0 {
		name += " (" + strings.Join(tail, ", ") + ")"
	}
	return name
}

// referenceQuery is the search for the free full text of a reference.
func referenceQuery(ref reference) string {
	var parts []string
	if ref.Title != "" {
		parts = append(parts, ref.Title)
		if s := surnames(ref); len(s) > 0 {
			parts = append(parts, s[0])
		}
	} else {
		parts = append(parts, surnames(ref)...)
		if ref.Year != "" {
			parts = append(parts, ref.Year)
		}
	}
	parts = append(parts, "pdf")
	return truncateRunes(strings.Join(parts, " "), maxWebQueryRunes)
}

// closedHosts give an abstract, a login or a refusal, not the full text of a paper.
var closedHosts = []string{"econpapers.repec.org", "ideas.repec.org", "ssrn.com", "jstor.org", "sciencedirect.com", "wiley.com",
	"tandfonline.com", "springer.com", "oup.com", "researchgate.net", "academia.edu", "google.com", "amazon.com", "scholar.archive.org"}

func closedHost(host string) bool {
	for _, h := range closedHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// fullTextLink is the address of a full text a result points to, "" when it points to none: a PDF,
// or a preprint of arXiv (whose page of the abstract becomes the address of its PDF).
func fullTextLink(link string) string {
	if _, err := validateFetchURL(link); err != nil {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if closedHost(host) {
		return ""
	}
	// The search marks a passage inside a PDF with "#<page>#<pages>#<page>": a page has no such part.
	u.Fragment, u.RawFragment = "", ""
	path := strings.ToLower(u.Path)
	switch {
	case host == "arxiv.org" && strings.HasPrefix(path, "/abs/"):
		u.Path = "/pdf/" + strings.TrimPrefix(u.Path, "/abs/")
		return u.String()
	case strings.HasSuffix(path, ".pdf"), strings.Contains(path, "/pdf/"), strings.HasSuffix(path, "/pdf"):
		return u.String()
	}
	return ""
}

// titleWords are the words of a title, lower case, letters and digits only.
func titleWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// maxExtraTitleWords: a result whose title has the reference's words and more than this many others is
// another work about it.
const maxExtraTitleWords = 4

// matchesReference tells whether a result is the work of the reference: its title is the reference's
// title, or begins with it (a list may give "Maxing out" for "Maxing out: Stocks as lotteries…"), or
// has every word of it and few others. Only the title counts, and a reference without one matches
// nothing: on the real list of 2026-11-28 the search gave, for a title or a name, passages from inside
// theses and slides that cite the work ("George and Hwang (2004) also suggest…", page 31 of 45),
// and a rule of "most words anywhere in the result" took 8 wrong documents of 10.
func matchesReference(h webHit, ref reference) bool {
	want := titleWords(ref.Title)
	if len(want) == 0 {
		return false
	}
	got := titleWords(strings.TrimPrefix(strings.TrimSpace(h.Title), "[PDF]"))
	if len(got) < len(want) {
		return false
	}
	if strings.Join(got[:len(want)], " ") == strings.Join(want, " ") && (len(want) >= 2 || len(got) == 1) {
		return true
	}
	if len(want) < 3 {
		return false // a title of a word or two is told by its start only
	}
	have := map[string]bool{}
	for _, w := range got {
		have[w] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return len(got)-len(want) <= maxExtraTitleWords
}

// pickFullText is the first result that is a full text of the reference and not its own link, "".
func pickFullText(hits []webHit, ref reference) string {
	own := urlKey(ref.Link)
	for _, h := range hits {
		link := fullTextLink(h.Link)
		if link == "" || (own != "" && urlKey(link) == own) || !matchesReference(h, ref) {
			continue
		}
		return link
	}
	return ""
}

// sourceFind is what became of one reference.
type sourceFind struct {
	Ref reference
	// Probe is the check of the person's own link; nil when the reference has none.
	Probe *linkProbe
	// Found is the full text the search found when the person's link gives no text of its own.
	Found string
	// Searched: the search was made (and gave nothing when Found is "").
	Searched bool
}

// referencesFor are the references the message is about: of the section it names of a file attached
// to the conversation, else of the person's newest message with a list (this one included), else of
// the newest attached file; and where they are from, in the person's words.
func (o *Orchestrator) referencesFor(ctx context.Context, userID uuid.UUID, session, message string, history []openai.ChatCompletionMessage) ([]reference, string) {
	section := ""
	if m := sectionNumber.FindStringSubmatch(message); m != nil {
		section = m[1] + m[2]
	}
	fromFile := func() ([]reference, string) {
		if o.attachments == nil {
			return nil, ""
		}
		files, err := o.attachments.Files(ctx, userID, session)
		if err != nil {
			slog.WarnContext(ctx, "could not read the attached files for their references", "err", err)
			return nil, ""
		}
		for _, f := range files {
			text, where := f.Content, "във файла „"+f.Name+"“"
			if section != "" {
				part, ok := sectionOf(text, section)
				if !ok {
					continue
				}
				text, where = part, "в раздел "+section+" на „"+f.Name+"“"
			}
			if refs := parseReferences(text); len(refs) >= 2 {
				return refs, where
			}
		}
		return nil, ""
	}
	if section != "" || referenceFileWords.MatchString(message) {
		if refs, where := fromFile(); refs != nil {
			return refs, where
		}
	}
	seen := 0
	for i := len(history) - 1; i >= 0 && seen < recentReferenceMessages; i-- {
		if history[i].Role != openai.ChatMessageRoleUser {
			continue
		}
		seen++
		if refs := parseReferences(withoutClosingNote(history[i].Content)); len(refs) >= 2 {
			where := "в съобщението ти"
			if seen > 1 {
				where = "в по-ранното ти съобщение"
			}
			return refs, where
		}
	}
	return fromFile()
}

// sectionHeading is the heading of a numbered section: "## 11. Източници", "11. Източници",
// "11-източници"; markdownHeading any heading of Markdown.
var (
	sectionHeading  = regexp.MustCompile(`^\s*(#{1,6})?\s*(?:раздел\s+)?(\d{1,2})\s*[.):\-–]?\s*\p{L}`)
	markdownHeading = regexp.MustCompile(`^\s*(#{1,6})\s+\S`)
)

// sectionOf is the text of section n of a document: from its heading to the next heading of its level
// or above (a Markdown one), or to the heading of the number after it (a plain "12. …").
func sectionOf(text, n string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start, level := -1, 0
	for i, l := range lines {
		if m := sectionHeading.FindStringSubmatch(l); m != nil && m[2] == n && (m[1] != "" || !listBullet.MatchString(l) || strings.Contains(l, n+"-")) {
			start, level = i, len(m[1])
			break
		}
	}
	if start < 0 {
		return "", false
	}
	next := nextNumber(n)
	var b strings.Builder
	for _, l := range lines[start+1:] {
		if level > 0 {
			if m := markdownHeading.FindStringSubmatch(l); m != nil && len(m[1]) <= level {
				break
			}
		} else if m := sectionHeading.FindStringSubmatch(l); m != nil && m[1] == "" && m[2] == next {
			break
		}
		b.WriteString(l + "\n")
	}
	return b.String(), true
}

func nextNumber(n string) string {
	var k int
	fmt.Sscanf(n, "%d", &k)
	return fmt.Sprint(k + 1)
}

// findSources checks the person's links of the references and searches for the rest.
func (o *Orchestrator) findSources(ctx context.Context, userID uuid.UUID, refs []reference, prober linkProber, searcher webSearcher) []sourceFind {
	finds := make([]sourceFind, len(refs))
	var links []string
	for i, r := range refs {
		finds[i].Ref = r
		if r.Link != "" {
			links = append(links, r.Link)
		}
	}
	if len(links) > 0 {
		probes := prober.probeLinks(ctx, userID, links)
		k := 0
		for i := range finds {
			if finds[i].Ref.Link != "" {
				p := probes[k]
				finds[i].Probe = &p
				k++
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, sourceSearchDeadline)
	defer cancel()
	slots := make(chan struct{}, addConcurrency)
	var wg sync.WaitGroup
	for i := range finds {
		f := &finds[i]
		if f.Probe != nil && f.Probe.Reason == "" {
			continue // the person's own link gives the document
		}
		if f.Ref.Title == "" {
			continue // nothing to tell the work from the theses that cite it (matchesReference)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			hits, err := searcher.searchWeb(ctx, referenceQuery(f.Ref))
			if err != nil {
				slog.InfoContext(ctx, "the search for a source failed", "err", err)
				return
			}
			f.Searched = true
			f.Found = pickFullText(hits, f.Ref)
		}()
	}
	wg.Wait()
	return finds
}

// sourceSearchAnswer is the answer of the code to a request to look for the sources of a list: the
// proposal, or why there is none; "" when the message is no such request or the server cannot do it.
func (o *Orchestrator) sourceSearchAnswer(ctx context.Context, userID uuid.UUID, session, message string, history []openai.ChatCompletionMessage) string {
	if o.runPlans == nil || !o.addToLibrary || !asksToFindSources(message) {
		return ""
	}
	prober, ok := o.tools.(linkProber)
	if !ok || !prober.canProbeLinks() {
		return ""
	}
	searcher, ok := o.tools.(webSearcher)
	if !ok {
		return ""
	}
	refs, where := o.referencesFor(ctx, userID, session, message, history)
	if len(refs) == 0 {
		return "" // no list of references: the model answers as before
	}
	more := 0
	if len(refs) > maxReferences {
		more = len(refs) - maxReferences
		refs = refs[:maxReferences]
	}
	kind, named := linkTaskKind(message)
	if kind == "" {
		kind, named = taskrun.KindKeyFacts, false
		if wholeWords.MatchString(message) {
			kind, named = taskrun.KindFull, true
		}
	}
	finds := o.findSources(ctx, userID, refs, prober, searcher)
	var steps []taskplan.Step
	for _, f := range finds {
		switch {
		case f.Probe != nil && f.Probe.Reason == "" && f.Probe.InLibraryID != uuid.Nil:
			steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: f.Probe.InLibrary, Ref: f.Probe.InLibraryID.String(), Status: taskplan.StepPending})
		case f.Probe != nil && f.Probe.Reason == "":
			steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: referenceName(f.Ref), Link: f.Ref.Link, Status: taskplan.StepPending})
		case f.Found != "":
			// The runner checks that the document it took is this work (its title at its start).
			steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: referenceName(f.Ref), Link: f.Found, Expect: f.Ref.Title, Status: taskplan.StepPending})
		}
	}
	metrics.TaskRuns.WithLabelValues(kind, "sources_searched").Inc()
	if len(steps) == 0 {
		metrics.TaskRuns.WithLabelValues(kind, "no_documents").Inc()
		return sourcesProposalText(kind, named, where, finds, more, false) + "\n\nЗатова нищо не започвам."
	}
	if _, err := o.runPlans.Propose(ctx, userID, session, kind, linkTaskGoal(kind, len(steps)), steps); err != nil {
		slog.WarnContext(ctx, "could not store the proposed task over the sources; the model answers", "err", err)
		return ""
	}
	metrics.TaskRuns.WithLabelValues(kind, "proposed").Inc()
	o.dropOtherProposals(ctx, userID, session, proposalTask)
	return sourcesProposalText(kind, named, where, finds, more, true)
}

// sourcesProposalText is what was found for each reference and, when something was, the question
// before the start.
func sourcesProposalText(kind string, named bool, where string, finds []sourceFind, more int, ask bool) string {
	var own, found, none []string
	for _, f := range finds {
		name := referenceName(f.Ref)
		switch {
		case f.Probe != nil && f.Probe.Reason == "":
			line := probeLine(*f.Probe, webfetch.DefaultMaxBytes)
			if f.Probe.InLibraryID != uuid.Nil {
				line += " — не го свалям пак"
			}
			own = append(own, "• "+name+" — "+line)
		case f.Found != "":
			why := "няма линк"
			if f.Probe != nil {
				why = "линкът ти: " + probeLine(*f.Probe, webfetch.DefaultMaxBytes)
			}
			found = append(found, "• "+name+" ("+why+")\n  → "+shortURL(f.Found))
		default:
			why := "няма линк"
			if f.Probe != nil {
				why = "линкът ти: " + probeLine(*f.Probe, webfetch.DefaultMaxBytes)
			}
			switch {
			case f.Searched:
				why += "; в търсенето няма свободен пълен текст със същото заглавие"
			case f.Ref.Title == "":
				why += "; без заглавие не търся — не бих могла да проверя, че намереното е същата статия"
			default:
				why += "; търсенето не стигна до нея"
			}
			none = append(none, "• "+name+" — "+why)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Справките %s са %d. Проверих линковете ти и за останалите потърсих в интернет свободен пълен текст (PDF):\n", where, len(finds))
	if len(own) > 0 {
		fmt.Fprintf(&b, "\nС твоя линк, който става (%d):\n%s\n", len(own), strings.Join(own, "\n"))
	}
	if len(found) > 0 {
		fmt.Fprintf(&b, "\nНамерих пълния текст другаде (%d) — тези адреси са от търсенето и ги отварям чак след твоето „да“:\n%s\n", len(found), strings.Join(found, "\n"))
	}
	if len(none) > 0 {
		fmt.Fprintf(&b, "\nНе намерих (%d):\n%s\n", len(none), strings.Join(none, "\n"))
	}
	if more > 0 {
		fmt.Fprintf(&b, "\n(Взимам първите %d справки; за останалите %d — после.)\n", len(finds), more)
	}
	if !ask {
		return strings.TrimRight(b.String(), "\n")
	}
	n := len(own) + len(found)
	noun := "файл с ключовите факти"
	switch kind {
	case taskrun.KindSummary:
		noun = "файл с резюме"
	case taskrun.KindFull:
		noun = "файл с подробни бележки по целия текст (на части)"
	}
	fmt.Fprintf(&b, "\nМога да сваля тези %d в библиотеката един по един и за всеки да направя %s. Върви на заден план (≈ %s); "+
		"документ, който не се свали, пропускам и казвам защо. Всеки файл е нов (нищо в папката не се заменя).\n\nДа започна ли?",
		n, noun, estimateSeconds(n*(taskrun.SecondsPerItem+ingestSecondsPerLink)))
	if !named {
		b.WriteString(" Ключовите факти са кратки (8–11 реда); ако искаш подробни бележки по целия текст на всеки, кажи „цялата“.")
	}
	return b.String()
}
