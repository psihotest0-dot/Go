package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// A task over the links of the conversation is walked by the server (2026-11-28). In the Flet chat
// of 2026-11-20 the owner asked "извлечеш информация от посочените линкове и за всеки да създадеш
// файл с информация" over 21 sources: the model went through the first one by hand, wrote a short
// synthesis ("прекалено синтезиран"), and "опитай да запишеш цялата статия" got "ще започна веднага"
// with nothing done. The walk over the documents added to the library (task_run.go) did not take
// it: the links were not in the library yet.
//
// Now such a message is answered by the code: the links of the person's last message with links
// are checked (link_probe.go), the proposal lists what each one is and which will not work, and the
// person's yes starts the walk: each link is put into the library the way add_to_library does
// (DefaultToolRegistry.AddLink), and its file is made from its passages once it is processed. A
// message that names no recipe gets the key facts, and the proposal says that "цялата" gives notes
// over the whole text instead (the owner's choice, 2026-11-28).

var (
	// linkTaskNoun: the links themselves are named ("за всеки линк", "посочените линкове").
	linkTaskNoun = regexp.MustCompile(`(?i)(?:^|\P{L})(?:линк\p{L}*|адрес\p{L}*|url\p{L}*|links?|urls?)(?:\P{L}|$)`)
	// addedWord: "добавените източници" are documents in the library already (task_run.go).
	addedWord = regexp.MustCompile(`(?i)добавен\p{L}*|\badded\b`)
)

// linkTaskKind is the recipe of a task over the links of the conversation and whether the message
// named it; "" when the message is no such task. It is one when it asks for work on each item and
// for a file, says nothing of the added documents, and names the links or holds them.
func linkTaskKind(message string) (kind string, named bool) {
	bare := strings.TrimSpace(urlPattern.ReplaceAllString(message, " "))
	if !asksForManySteps(bare) || !(fileMention.MatchString(bare) || asksToSave(bare)) || addedWord.MatchString(bare) {
		return "", false
	}
	if !linkTaskNoun.MatchString(bare) && !pageLink.MatchString(message) {
		return "", false
	}
	switch {
	case wholeWords.MatchString(bare):
		return taskrun.KindFull, true
	case keyFactsWords.MatchString(bare):
		return taskrun.KindKeyFacts, true
	case summaryWords.MatchString(bare):
		return taskrun.KindSummary, true
	}
	return taskrun.KindKeyFacts, false
}

// noLinksForTaskText answers a task over links in a conversation where the person wrote none.
const noLinksForTaskText = "Не виждам линкове, написани в този разговор (линкове в прикачен файл не тегля). " +
	"Постави ги в съобщение — може и много наведнъж — и повтори молбата: ще ти покажа какво е всеки и ще те питам, преди да започна."

// linkTaskAnswer is the answer of the code to a task over the links of the conversation: the proposal,
// or why there is none; "" when the message is no such task or the server cannot walk it.
func (o *Orchestrator) linkTaskAnswer(ctx context.Context, userID uuid.UUID, session, message string, history []openai.ChatCompletionMessage) string {
	if o.runPlans == nil || !o.addToLibrary {
		return ""
	}
	kind, named := linkTaskKind(message)
	if kind == "" {
		return ""
	}
	prober, ok := o.tools.(linkProber)
	if !ok || !prober.canProbeLinks() {
		return ""
	}
	links, more := lastUserLinks(history)
	if len(links) == 0 {
		metrics.TaskRuns.WithLabelValues(kind, "no_links").Inc()
		return noLinksForTaskText
	}
	probes := prober.probeLinks(ctx, userID, links)
	var steps []taskplan.Step
	for _, p := range probes {
		switch {
		case p.Reason != "":
			continue
		case p.InLibraryID != uuid.Nil:
			// Already in the library: its file is made from it, nothing is fetched again.
			steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: p.InLibrary, Ref: p.InLibraryID.String(), Status: taskplan.StepPending})
		default:
			steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: probeName(p), Link: p.Link, Status: taskplan.StepPending})
		}
	}
	if len(steps) == 0 {
		metrics.TaskRuns.WithLabelValues(kind, "no_documents").Inc()
		return linkProbeText(probes, more, webfetch.DefaultMaxBytes) + "\n\nЗатова нищо не започвам."
	}
	if _, err := o.runPlans.Propose(ctx, userID, session, kind, linkTaskGoal(kind, len(steps)), steps); err != nil {
		slog.WarnContext(ctx, "could not store the proposed task over links; the model answers", "err", err)
		return ""
	}
	metrics.TaskRuns.WithLabelValues(kind, "proposed").Inc()
	// The "да" that follows is for the task, not for a bill or a reminder that waited.
	o.dropOtherProposals(ctx, userID, session, proposalTask)
	return linkProposalText(kind, named, probes, more, len(steps))
}

// probeName is how a checked link is named in a list: its title, else its host.
func probeName(p linkProbe) string {
	if p.Title != "" {
		return truncateRunes(p.Title, 90)
	}
	return hostOf(p.Link)
}

// linkTaskGoal is the goal of a plan over links, the way the report and the status answer name it.
func linkTaskGoal(kind string, n int) string {
	switch kind {
	case taskrun.KindFull:
		return fmt.Sprintf("подробни бележки по целия текст на всеки от %d линка", n)
	case taskrun.KindSummary:
		return fmt.Sprintf("резюме на всеки от %d линка", n)
	}
	return fmt.Sprintf("ключовите факти на всеки от %d линка", n)
}

// ingestSecondsPerLink is the time of a link before its file: the download and the processing in the
// library (the passages and their vectors), for the estimate.
const ingestSecondsPerLink = 60

// linkProposalText is the question before the start of a task over links.
func linkProposalText(kind string, named bool, probes []linkProbe, more bool, steps int) string {
	noun := "файл с ключовите факти"
	switch kind {
	case taskrun.KindSummary:
		noun = "файл с резюме"
	case taskrun.KindFull:
		noun = "файл с подробни бележки по целия текст (чета го от началото до края, на части)"
	}
	fetched := 0
	for _, p := range probes {
		if p.Reason == "" && p.InLibraryID == uuid.Nil {
			fetched++
		}
	}
	var b strings.Builder
	if fetched == 0 {
		// Every link that works is in the library already: nothing is downloaded.
		fmt.Fprintf(&b, "Мога да направя %s за всеки — документите им вече са в библиотеката. Проверих ги (%d):\n", noun, len(probes))
	} else {
		fmt.Fprintf(&b, "Мога да сваля линковете в библиотеката един по един и за всеки да направя %s. Проверих ги (%d):\n", noun, len(probes))
	}
	var failed []linkProbe
	for _, p := range probes {
		if p.Reason != "" {
			failed = append(failed, p)
			continue
		}
		line := probeLine(p, webfetch.DefaultMaxBytes)
		if p.InLibraryID != uuid.Nil {
			line += " — не го свалям пак"
		}
		b.WriteString("• " + probeName(p) + " — " + line + "\n")
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\nНе стават (%d), тях пропускам:\n", len(failed))
		for _, p := range failed {
			b.WriteString("• " + probeName(p) + " — " + probeLine(p, webfetch.DefaultMaxBytes) + "\n")
		}
	}
	if more {
		fmt.Fprintf(&b, "\n(Взимам първите %d линка; за останалите — после, в отделно съобщение.)\n", maxProbeLinks)
	}
	fetch := "всеки линк се сваля и обработва в библиотеката, и "
	if fetched == 0 {
		fetch = ""
	}
	if kind == taskrun.KindFull {
		if fetched > 0 {
			fetch = "всеки линк се сваля и обработва (≈ минута), после "
		}
		fmt.Fprintf(&b, "\nВърви на заден план: %sчета текста на части (около %d s на част; статия от 20 страници ≈ 2 минути). "+
			"Бележките растат във файла след всяка част; всяко число се проверява срещу текста.", fetch, taskrun.SecondsPerPart)
	} else {
		fmt.Fprintf(&b, "\nЩе отнеме около %s и върви на заден план: %sфайлът на всеки се пише само от неговите откъси; "+
			"числата се проверяват срещу тях.", estimateSeconds(steps*taskrun.SecondsPerItem+fetched*ingestSecondsPerLink), fetch)
	}
	b.WriteString(" Всеки файл е нов (нищо в папката не се заменя).\n\nДа започна ли?")
	if !named {
		b.WriteString(" Ключовите факти са кратки (8–11 реда); ако искаш подробни бележки по целия текст на всяка статия, кажи „цялата“.")
	}
	return b.String()
}

// estimateSeconds is a time in the person's words.
func estimateSeconds(seconds int) string {
	minutes := (seconds + 59) / 60
	if minutes <= 1 {
		return "минута"
	}
	return fmt.Sprintf("%d минути", minutes)
}

// isWholeReply: "цялата", "целия текст" to a proposal - the notes over the whole text instead.
func isWholeReply(message string) bool {
	m, ok := shortReply(message)
	return ok && wholeWords.MatchString(m) && !notWord.MatchString(m)
}

// AddLink puts the document behind a link into the library for a task the server walks
// (taskrun.LinkAdder): the road of add_to_library (the same fetcher, the Wayback copy as the last
// one). The link is one the person wrote (the steps are made from their message), or one the search
// found for a reference of theirs and they said yes to in the proposal that showed it
// (source_search.go). The error's text is the reason in the person's words.
func (r *DefaultToolRegistry) AddLink(ctx context.Context, userID uuid.UUID, link string) (uuid.UUID, string, error) {
	if r.fetcher == nil || r.adder == nil {
		return uuid.Nil, "", errors.New("свалянето на линкове е изключено")
	}
	ctx, cancel := context.WithTimeout(ctx, addOneDeadline)
	defer cancel()
	if known, ok := r.librarySource(ctx, userID, link); ok {
		return known.ID, known.FileName, nil // in the library already (library_source.go)
	}
	doc, err := r.fetcher.Fetch(ctx, link)
	if err != nil {
		copied, _ := r.archivedDocument(ctx, link, err)
		if copied == nil {
			reason := probeReason(ctx, err)
			metrics.LibraryAdd.WithLabelValues(reason).Inc()
			return uuid.Nil, "", errors.New(linkReasonWords(reason))
		}
		doc = copied
	}
	defer doc.Close()
	added, err := r.adder.Add(ctx, userID, libraryFileName("", doc.Name), doc.Body)
	switch {
	case errors.Is(err, webfetch.ErrTooLarge), errors.Is(err, api.ErrAddTooLarge):
		metrics.LibraryAdd.WithLabelValues(string(webfetch.ReasonTooLarge)).Inc()
		return uuid.Nil, "", errors.New(linkReasonWords(string(webfetch.ReasonTooLarge)))
	case errors.Is(err, api.ErrAddEmpty):
		metrics.LibraryAdd.WithLabelValues("empty").Inc()
		return uuid.Nil, "", errors.New("документът е празен")
	case err != nil && ctx.Err() != nil:
		metrics.LibraryAdd.WithLabelValues(string(webfetch.ReasonTimeout)).Inc()
		return uuid.Nil, "", errors.New(linkReasonWords(string(webfetch.ReasonTimeout)))
	case err != nil:
		// A failure of this server, not of the link: logged in full, said in short.
		slog.ErrorContext(ctx, "a link of a task could not be stored", "host", hostOf(link), "error", err)
		metrics.LibraryAdd.WithLabelValues("error").Inc()
		return uuid.Nil, "", errors.New("сървърът не можа да запише документа")
	}
	r.rememberSource(ctx, userID, added.ID, link)
	if added.Duplicate {
		metrics.LibraryAdd.WithLabelValues("duplicate").Inc()
	} else {
		metrics.LibraryAdd.WithLabelValues("queued").Inc()
	}
	slog.InfoContext(ctx, "a link of a task is in the library", "host", hostOf(link), "kind", doc.Kind, "duplicate", added.Duplicate)
	return added.ID, added.Name, nil
}

// linkReasonWords is the reason of a link that gave nothing, in the person's words.
func linkReasonWords(reason string) string {
	if reason == string(webfetch.ReasonTooLarge) {
		return fmt.Sprintf("по-голям от %s — не мога да го сваля", sizeWords(webfetch.DefaultMaxBytes))
	}
	if words, ok := probeReasonWords[reason]; ok {
		return words
	}
	return "не се отваря"
}
