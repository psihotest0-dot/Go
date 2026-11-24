package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
	"go-monolith/metrics"
)

// The start of a task the server walks itself (plan step 3.3; the walk is package taskrun).
//
// "За всеки от добавените източници направи отделен файл с ключовите му факти" is answered by the
// code, not by the model: the documents are the ones added to the library in this conversation,
// the recipe is the one the message names (key facts, a summary), and the answer is the list with
// the question "да започна ли?". The person's yes starts the walk in the background (the owner,
// 2026-11-23: that yes is enough for every file of the list); "спри" stops it; "докъде стигна?"
// is answered from the plan (status_answer.go). Any other message drops the proposal, and the
// model answers it as before. Off unless TASK_RUN_ENABLED (WithTaskRun).

const (
	// maxReplyRunes: a yes, a no or a stop is short; a longer message says more than that.
	maxReplyRunes = 40
	// taskRunListed is how many documents the proposal names; the rest are counted.
	taskRunListed = 30
)

var (
	// wholeWords: the whole of each document is to be read, in parts (taskrun.KindFull): "изцяло",
	// "целия документ", "на части", "подробни бележки", "всичко от", "in full".
	wholeWords = regexp.MustCompile(`(?i)(?:изцяло|целия|целият|цялата|цялото|на\s+части|част\s+по\s+част|от\s+начало(?:то)?\s+до\s+края?|подробн\p{L}*\s+(?:бележк|извлеч|записк)|извлечи\s+всичко|всичко\s+от|\bin\s+full\b|\bwhole\b|\bentire\b|detailed\s+notes)`)
	// keyFactsWords and summaryWords say what is to be made of each document.
	keyFactsWords = regexp.MustCompile(`(?i)(?:ключов\p{L}*\s+(?:\p{L}{1,3}\s+)?(?:факт|точк|момент|извод|данн)|основн\p{L}*\s+(?:\p{L}{1,3}\s+)?(?:факт|извод|данн)|key\s+(?:facts|points|findings)|main\s+findings)`)
	summaryWords  = regexp.MustCompile(`(?i)(?:резюм|обобщ|summar)`)
	// yesWords, noWords, stopWords are the short answers. A "не" anywhere makes a yes no yes.
	yesWords  = regexp.MustCompile(`(?i)^(?:да|дa|давай|започни|започвай|започвайте|почни|пускай|пусни\s+го|хайде|добре|ок|окей|ok|okay|yes|yep|go|разбира\s+се|естествено|може|действай)(?:[\s,!.]|$)`)
	noWords   = regexp.MustCompile(`(?i)^(?:не|недей|няма\s+нужда|откажи|отказ|забрави|no|nope|cancel)(?:[\s,!.]|$)`)
	stopWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:спри|стоп|прекрати|прекъсни|откажи\s+(?:задачата|работата)|stop|cancel)(?:[^\p{L}]|$)`)
	notWord   = regexp.MustCompile(`(?i)(?:^|[^\p{L}])не(?:[^\p{L}]|$)`)
	// libraryItems: the message is about documents of the library (added ones, sources), not
	// about the files of the folder.
	libraryItems = regexp.MustCompile(`(?i)(добав|източни[кц]|библиотек|\badded\b|\bsources?\b|\blibrary\b)`)
)

// noAddedDocumentsText answers a task over the added documents in a conversation that added none.
const noAddedDocumentsText = "В този разговор не е добавен нито един документ в библиотеката, а такава задача правя само върху документите, добавени тук. " +
	"Първо ме помоли: „Добави в библиотеката: <линк>“ (може и няколко линка наведнъж), после пак ми кажи какво да направя с тях."

// taskRunWaker is the runner of the tasks (taskrun.Runner).
type taskRunWaker interface{ Wake() }

// WithTaskRun lets the server walk tasks itself: plans keeps their plans, runner walks them.
func (o *Orchestrator) WithTaskRun(plans *taskplan.Store, runner taskRunWaker) *Orchestrator {
	o.runPlans, o.runner = plans, runner
	return o
}

// taskRunKind is the recipe a message asks for over many documents: "" when it asks for none.
func taskRunKind(message string) string {
	if !asksForManySteps(message) || !(fileMention.MatchString(message) || asksToSave(message)) {
		return ""
	}
	switch {
	case wholeWords.MatchString(message):
		return taskrun.KindFull
	case keyFactsWords.MatchString(message):
		return taskrun.KindKeyFacts
	case summaryWords.MatchString(message):
		return taskrun.KindSummary
	}
	return ""
}

var (
	// oneDocumentNoun, notesWords: the reading of ONE whole document asks for notes ("подробни
	// бележки", "извлечи всичко") of a document ("статията", "документа", "книгата", "PDF-а").
	oneDocumentNoun = regexp.MustCompile(`(?i)(?:статия|статията|документ\p{L}*|книга|книгата|текст\p{L}*|източник\p{L}*|pdf\p{L}*|\barticle\b|\bpaper\b|\bdocument\b|\bbook\b)`)
	notesWords      = regexp.MustCompile(`(?i)(?:бележк|извлеч|записк|\bnotes\b|\bextract)`)
)

// wholeOneDocument tells whether a message asks for the whole of one document read into a file of
// notes (taskrun.KindFull over the newest document added in the conversation): "прочети цялата
// статия и запиши подробни бележки във файл". It needs the whole, a document, notes and a file or
// a word for saving, and no link: "запиши цялата статия във файл: <линк>" is the page word for word
// (save_page), and a link is still to be added (add_to_library).
func wholeOneDocument(message string) bool {
	if utf8.RuneCountInString(message) > maxMultiStepRunes || pageLink.MatchString(message) {
		return false
	}
	return wholeWords.MatchString(message) && oneDocumentNoun.MatchString(message) && notesWords.MatchString(message) &&
		(fileMention.MatchString(message) || asksToSave(message))
}

// shortReply is the message trimmed, when it is short enough to be a yes, a no or a stop.
func shortReply(message string) (string, bool) {
	message = strings.TrimSpace(message)
	return message, message != "" && utf8.RuneCountInString(message) <= maxReplyRunes
}

func isYes(message string) bool {
	m, ok := shortReply(message)
	return ok && yesWords.MatchString(m) && !notWord.MatchString(m)
}

func isNo(message string) bool {
	m, ok := shortReply(message)
	return ok && noWords.MatchString(m)
}

func isStop(message string) bool {
	m, ok := shortReply(message)
	return ok && stopWords.MatchString(m)
}

// taskRunAnswer is the answer of the code to the person's message about a task the server walks,
// "" when the model is to answer it. since is the time of the first message of the history the
// turn reads: the documents added from then on are "the added ones".
func (o *Orchestrator) taskRunAnswer(ctx context.Context, userID uuid.UUID, session, message string, since time.Time) string {
	if o.runPlans == nil {
		return ""
	}
	running, err := o.runPlans.Running(ctx, userID, session)
	if err != nil {
		slog.WarnContext(ctx, "could not read the running task; the model answers", "err", err)
		return ""
	}
	if running != nil {
		if isStop(message) {
			if stopped, err := o.runPlans.StopRun(ctx, userID, session); err != nil || !stopped {
				return ""
			}
			metrics.TaskRuns.WithLabelValues(running.Kind, "stopped").Inc()
			return "Спрях задачата. " + planStatusText(running) + "\nФайловете, които са готови, остават в папката."
		}
		if isYes(message) {
			return "Вече работя по нея. " + planStatusText(running)
		}
	}

	proposed, err := o.runPlans.Proposed(ctx, userID, session)
	if err != nil {
		slog.WarnContext(ctx, "could not read the proposed task; the model answers", "err", err)
		return ""
	}
	if proposed != nil {
		switch {
		case isYes(message):
			started, err := o.runPlans.Start(ctx, userID, proposed.ID)
			if err != nil {
				slog.WarnContext(ctx, "the task was not started", "err", err)
				return "Не започнах: в този разговор вече върви такава задача. Кажи „спри“, ако искаш да я спреш."
			}
			metrics.TaskRuns.WithLabelValues(started.Kind, "started").Inc()
			if o.runner != nil {
				o.runner.Wake()
			}
			slog.InfoContext(ctx, "a task of the server was started", "plan_id", started.ID, "kind", started.Kind, "steps", len(started.Steps))
			return fmt.Sprintf("Започнах. Работя на заден план, по един документ (≈ %s). Когато свърша, ще пиша тук какво е записано и какво не е станало. "+
				"Междувременно можеш да ме питаш „докъде стигна?“ или да кажеш „спри“.", estimate(len(started.Steps)))
		case isNo(message):
			_ = o.runPlans.DropProposed(ctx, userID, session)
			metrics.TaskRuns.WithLabelValues(proposed.Kind, "declined").Inc()
			return "Добре, няма да го правя."
		default:
			// Something else was asked: the proposal is not left to be started by a later "да".
			_ = o.runPlans.DropProposed(ctx, userID, session)
			metrics.TaskRuns.WithLabelValues(proposed.Kind, "declined").Inc()
		}
	}

	kind := taskRunKind(message)
	one := false
	if kind == "" && wholeOneDocument(message) {
		kind, one = taskrun.KindFull, true
	}
	if kind == "" || since.IsZero() {
		return ""
	}
	steps, names, err := o.addedDocuments(ctx, userID, since)
	if err != nil {
		slog.WarnContext(ctx, "could not list the documents of the task; the model answers", "err", err)
		return ""
	}
	if one && len(names) > 0 {
		// One document: the newest one added in the conversation (the steps hold at most
		// taskplan.MaxSteps of the oldest, so it is taken from the names).
		steps, names, err = o.newestAddedDocument(ctx, userID, since)
		if err != nil {
			slog.WarnContext(ctx, "could not list the documents of the task; the model answers", "err", err)
			return ""
		}
	}
	if len(steps) == 0 {
		if !one && !libraryItems.MatchString(message) {
			// "за всеки от файловете в папката…": not the library; the model answers.
			return ""
		}
		// Left to the model, such a message got "Създадох два нови файла" with nothing made (the
		// live check of 2026-11-24): the walk is for the documents added here, so the code says so.
		metrics.TaskRuns.WithLabelValues(kind, "no_documents").Inc()
		return noAddedDocumentsText
	}
	goal := taskRunGoal(kind, len(steps))
	if _, err := o.runPlans.Propose(ctx, userID, session, kind, goal, steps); err != nil {
		slog.WarnContext(ctx, "could not store the proposed task; the model answers", "err", err)
		return ""
	}
	metrics.TaskRuns.WithLabelValues(kind, "proposed").Inc()
	return proposalText(kind, names, len(steps))
}

// addedDocuments are the steps of a task over the documents added to the library since the time
// given (at most taskplan.MaxSteps, the oldest first), and the names of all of them.
func (o *Orchestrator) addedDocuments(ctx context.Context, userID uuid.UUID, since time.Time) ([]taskplan.Step, []string, error) {
	rows, err := o.db.Queries.GetIngestedFiles(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	var steps []taskplan.Step
	var names []string
	// The rows come the newest first.
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if !r.CreatedAt.Valid || r.CreatedAt.Time.Before(since) || r.Status == "failed" {
			continue
		}
		names = append(names, r.FileName)
		if len(steps) < taskplan.MaxSteps {
			steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: r.FileName, Ref: r.ID.String(), Status: taskplan.StepPending})
		}
	}
	return steps, names, nil
}

// newestAddedDocument is the step of the document added last since the time given, and its name.
func (o *Orchestrator) newestAddedDocument(ctx context.Context, userID uuid.UUID, since time.Time) ([]taskplan.Step, []string, error) {
	rows, err := o.db.Queries.GetIngestedFiles(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	// The rows come the newest first.
	for _, r := range rows {
		if !r.CreatedAt.Valid || r.CreatedAt.Time.Before(since) || r.Status == "failed" {
			continue
		}
		return []taskplan.Step{{ID: 1, Text: r.FileName, Ref: r.ID.String(), Status: taskplan.StepPending}}, []string{r.FileName}, nil
	}
	return nil, nil, nil
}

// taskRunGoal is the goal of the plan, the way the report and the status answer name it.
func taskRunGoal(kind string, n int) string {
	if kind == taskrun.KindFull {
		if n == 1 {
			return "подробни бележки по целия текст на документа"
		}
		return fmt.Sprintf("подробни бележки по целия текст на всеки от %d документа", n)
	}
	if kind == taskrun.KindSummary {
		return fmt.Sprintf("резюме на всеки от %d документа", n)
	}
	return fmt.Sprintf("ключовите факти на всеки от %d документа", n)
}

// estimate is the time of n documents in the person's words.
func estimate(n int) string {
	minutes := (n*taskrun.SecondsPerItem + 59) / 60
	if minutes <= 1 {
		return "минута"
	}
	return fmt.Sprintf("%d минути", minutes)
}

// proposalText is the question before the start.
func proposalText(kind string, names []string, steps int) string {
	noun := "файл с ключовите факти"
	switch kind {
	case taskrun.KindSummary:
		noun = "файл с резюме"
	case taskrun.KindFull:
		noun = "файл с подробни бележки по целия текст (чета го от началото до края, на части по ~10 000 знака)"
	}
	var b strings.Builder
	if steps == 1 {
		fmt.Fprintf(&b, "Мога да направя %s за документа, добавен в библиотеката в този разговор:\n", noun)
	} else {
		fmt.Fprintf(&b, "Мога да направя %s за всеки от тези %d документа, добавени в библиотеката в този разговор:\n", noun, steps)
	}
	for i, n := range names {
		if i == steps {
			break
		}
		if i == taskRunListed {
			fmt.Fprintf(&b, "• и още %d\n", steps-taskRunListed)
			break
		}
		b.WriteString("• " + n + "\n")
	}
	if len(names) > steps {
		fmt.Fprintf(&b, "(Наведнъж правя до %d; останалите %d — след това.)\n", steps, len(names)-steps)
	}
	if kind == taskrun.KindFull {
		// The length is known only after the ingest: the estimate is per part.
		fmt.Fprintf(&b, "\nВърви на заден план, около %d s на част (статия от 20 страници ≈ 6–8 части, ≈ 2 минути). Бележките се допълват във файла след всяка част, "+
			"така че го виждаш да расте; всяко число се проверява срещу текста на своята част. Документ над %d части (≈ %d знака — дълга книга) не чета целия. "+
			"Всеки файл е нов (нищо в папката не се заменя). Документ, който още се обработва, изчаквам.\n\nДа започна ли?", taskrun.SecondsPerPart, taskrun.MaxParts, taskrun.MaxParts*taskrun.PartRunes)
		return b.String()
	}
	fmt.Fprintf(&b, "\nЩе отнеме около %s и върви на заден план. Всеки файл е нов (нищо в папката не се заменя) и е писан само от откъсите на своя документ; "+
		"числата се проверяват срещу тях. Документ, който още се обработва, изчаквам.\n\nДа започна ли?", estimate(steps))
	return b.String()
}

// historyStart is the time of the first message of the history the turn reads, or the zero time.
func historyStart(history []openai.ChatCompletionMessage, at []time.Time) time.Time {
	if len(at) != len(history) {
		return time.Time{}
	}
	for _, t := range at {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// runningPlan is the plan the server walks in the conversation, for the answer to "докъде стигна?".
func (o *Orchestrator) runningPlan(ctx context.Context, userID uuid.UUID, session string) *taskplan.Plan {
	if o.runPlans == nil {
		return nil
	}
	p, err := o.runPlans.Running(ctx, userID, session)
	if err != nil {
		slog.WarnContext(ctx, "could not read the running task", "err", err)
		return nil
	}
	return p
}

func (t *turnState) markTaskRunning() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.taskRunning = true
}

// taskRunningNow: the server walks a task of this conversation, so "работя на заден план" is true.
func (t *turnState) taskRunningNow() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.taskRunning
}
