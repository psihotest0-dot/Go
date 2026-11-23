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
	// keyFactsWords and summaryWords say what is to be made of each document.
	keyFactsWords = regexp.MustCompile(`(?i)(?:ключов\p{L}*\s+(?:\p{L}{1,3}\s+)?(?:факт|точк|момент|извод|данн)|основн\p{L}*\s+(?:\p{L}{1,3}\s+)?(?:факт|извод|данн)|key\s+(?:facts|points|findings)|main\s+findings)`)
	summaryWords  = regexp.MustCompile(`(?i)(?:резюм|обобщ|summar)`)
	// yesWords, noWords, stopWords are the short answers. A "не" anywhere makes a yes no yes.
	yesWords  = regexp.MustCompile(`(?i)^(?:да|дa|давай|започни|започвай|започвайте|почни|пускай|пусни\s+го|хайде|добре|ок|окей|ok|okay|yes|yep|go|разбира\s+се|естествено|може|действай)(?:[\s,!.]|$)`)
	noWords   = regexp.MustCompile(`(?i)^(?:не|недей|няма\s+нужда|откажи|отказ|забрави|no|nope|cancel)(?:[\s,!.]|$)`)
	stopWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:спри|стоп|прекрати|прекъсни|откажи\s+(?:задачата|работата)|stop|cancel)(?:[^\p{L}]|$)`)
	notWord   = regexp.MustCompile(`(?i)(?:^|[^\p{L}])не(?:[^\p{L}]|$)`)
)

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
	case keyFactsWords.MatchString(message):
		return taskrun.KindKeyFacts
	case summaryWords.MatchString(message):
		return taskrun.KindSummary
	}
	return ""
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
	if kind == "" || since.IsZero() {
		return ""
	}
	steps, names, err := o.addedDocuments(ctx, userID, since)
	if err != nil {
		slog.WarnContext(ctx, "could not list the documents of the task; the model answers", "err", err)
		return ""
	}
	if len(steps) == 0 {
		return ""
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

// taskRunGoal is the goal of the plan, the way the report and the status answer name it.
func taskRunGoal(kind string, n int) string {
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
	if kind == taskrun.KindSummary {
		noun = "файл с резюме"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Мога да направя %s за всеки от тези %d документа, добавени в библиотеката в този разговор:\n", noun, steps)
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
