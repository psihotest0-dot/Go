package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
	"go-monolith/metrics"
)

// The answer to "успя ли?" from the code (plan step 3.2, И6). A question about how far some work
// is has nothing in it for a model to think about: the answer is in the folder and in the plan.
// Left to the model it was a guess - on 8 replays of the real turn of 2026-11-20 "Да, успях…" in
// 4 and "Не успях" in 3, with the file untouched (P54) - and P54 only put a correction under the
// wrong ones. Here the code answers by itself, and the model is not asked:
//
//   - with a plan of a task in progress (TASK_PLAN_ENABLED): how many steps are done, which
//     failed, which is next;
//   - else, when the work asked for was about the files (the request or the last answer speaks of
//     a file, or of saving): the files changed since the person's request, with their times and
//     sizes - or that none was, so nothing was written.
//
// Only a short question that asks nothing more ("успя ли?", "готово ли е?", "докъде стигна с
// файловете") is answered so; "успя ли? запиши го тогава в бележки.md" is a request and goes to the
// model. A question about other work ("успя ли да намериш книгата?") goes to the model too: the
// folder does not tell it. Off unless STATUS_ANSWER_ENABLED (WithStatusAnswer).

const (
	// statusAnswerMaxRunes: a longer message carries more than the question.
	statusAnswerMaxRunes = 80
	// statusAnswerMaxFiles is the most files the answer lists.
	statusAnswerMaxFiles = 10
)

// filesLister is a tool registry that can list the user's files.
type filesLister interface {
	ListUserFiles(userID uuid.UUID) ([]userfiles.Info, error)
}

// ListUserFiles is the user's files; none when the file tools are off.
func (r *DefaultToolRegistry) ListUserFiles(userID uuid.UUID) ([]userfiles.Info, error) {
	if r.files == nil {
		return nil, nil
	}
	return r.files.List(userID)
}

// WithStatusAnswer turns on the answer to a question about the work from the code.
func (o *Orchestrator) WithStatusAnswer() *Orchestrator {
	o.statusAnswer = true
	return o
}

// statusAnswerFor is the answer of the code to the last message of the history, "" when the
// model is to answer it. at are the times of the history's messages; the last one is the question.
// message is the person's message as written (the history's copy carries the notes for the model).
func (o *Orchestrator) statusAnswerFor(ctx context.Context, userID uuid.UUID, message string, history []openai.ChatCompletionMessage, at []time.Time, plan *taskplan.Plan) string {
	if !o.statusAnswer || len(history) == 0 || history[len(history)-1].Role != openai.ChatMessageRoleUser {
		return ""
	}
	message = strings.TrimSpace(message)
	if !asksForStatus(message) || utf8.RuneCountInString(message) > statusAnswerMaxRunes || asksToWrite(message) || asksToRemoveFile(message) {
		return ""
	}
	if plan != nil {
		metrics.UserFileOps.WithLabelValues("status", "plan").Inc()
		return planStatusText(plan)
	}
	if !o.hasFileGroup() {
		return ""
	}
	request := statusRequestIndex(history, at)
	if request < 0 || !aboutFiles(history, request) {
		return ""
	}
	lister, ok := o.tools.(filesLister)
	if !ok {
		return ""
	}
	files, err := lister.ListUserFiles(userID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the files for the answer about the work; the model answers", "err", err)
		return ""
	}
	since := at[request]
	changed := slices.DeleteFunc(slices.Clone(files), func(f userfiles.Info) bool { return f.Modified.Before(since) })
	slices.SortFunc(changed, func(a, b userfiles.Info) int { return a.Modified.Compare(b.Modified) })
	if len(changed) == 0 {
		metrics.UserFileOps.WithLabelValues("status", "nothing").Inc()
	} else {
		metrics.UserFileOps.WithLabelValues("status", "files").Inc()
	}
	return filesStatusText(changed, since, time.Now())
}

// statusRequestIndex is the index of the message requestSince takes the time of: the last
// message of the person before the question that is long enough to be a request, or the earliest
// one before it; -1 when there is none.
func statusRequestIndex(history []openai.ChatCompletionMessage, at []time.Time) int {
	if len(at) != len(history) {
		return -1
	}
	var users []int
	for i, m := range history {
		if m.Role == openai.ChatMessageRoleUser {
			users = append(users, i)
		}
	}
	if len(users) < 2 {
		return -1
	}
	earlier := users[:len(users)-1]
	for i := len(earlier) - 1; i >= 0; i-- {
		if utf8.RuneCountInString(strings.TrimSpace(history[earlier[i]].Content)) >= minRequestRunes {
			if at[earlier[i]].IsZero() {
				return -1
			}
			return earlier[i]
		}
	}
	if at[earlier[0]].IsZero() {
		return -1
	}
	return earlier[0]
}

// aboutFiles tells whether the work the question asks about is work on the files: the request
// asks to write or names a file, or an answer after it speaks of a file or promised work.
func aboutFiles(history []openai.ChatCompletionMessage, request int) bool {
	if text := history[request].Content; asksToWrite(text) || fileMention.MatchString(text) {
		return true
	}
	for _, m := range history[request+1 : len(history)-1] {
		if m.Role == openai.ChatMessageRoleAssistant && (fileMention.MatchString(m.Content) || promisesWork(m.Content)) {
			return true
		}
	}
	return false
}

// clock is a time as the answer gives it: the hour, and the day when it is not today.
func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("02.01 15:04")
}

// size is the size of a file in words a person reads.
func size(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d байта", n)
	case n < 1024*1024:
		return strings.Replace(fmt.Sprintf("%.1f KB", float64(n)/1024), ".", ",", 1)
	default:
		return strings.Replace(fmt.Sprintf("%.1f MB", float64(n)/1024/1024), ".", ",", 1)
	}
}

// filesStatusText is the answer about the files changed since the request.
func filesStatusText(changed []userfiles.Info, since, now time.Time) string {
	if len(changed) == 0 {
		return "Не. От молбата ти в " + clock(since, now) + " нито един файл не е променян — нищо не е записано. " +
			"Кажи ми да го направя сега и го правя в самия отговор."
	}
	var b strings.Builder
	b.WriteString("Ето какво се е променило във файловете от молбата ти в " + clock(since, now) + " насам:\n")
	for i, f := range changed {
		if i == statusAnswerMaxFiles {
			fmt.Fprintf(&b, "• и още %d\n", len(changed)-statusAnswerMaxFiles)
			break
		}
		fmt.Fprintf(&b, "• %s — в %s, %s\n", f.Name, clock(f.Modified, now), size(f.Size))
	}
	b.WriteString("Това са истинските промени в папката. Ако нещо липсва, кажи ми и продължавам.")
	return b.String()
}

// planStatusText is the answer about a plan in progress.
func planStatusText(p *taskplan.Plan) string {
	var done, failed, skipped int
	var failedSteps []string
	for _, s := range p.Steps {
		switch s.Status {
		case taskplan.StepDone:
			done++
		case taskplan.StepFailed:
			failed++
			failedSteps = append(failedSteps, fmt.Sprintf("%d. %s", s.ID, s.Text))
		case taskplan.StepSkipped:
			skipped++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Планът „%s“: готови %d от %d стъпки", p.Goal, done, len(p.Steps))
	if failed > 0 {
		fmt.Fprintf(&b, ", неуспешни %d", failed)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, ", пропуснати %d", skipped)
	}
	b.WriteString(".")
	for _, s := range failedSteps {
		b.WriteString("\n• не стана: " + s)
	}
	if next := p.Next(); next != nil {
		fmt.Fprintf(&b, "\nСледва: %d. %s", next.ID, next.Text)
	}
	return b.String()
}

func (t *turnState) setStatusAnswer(text string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.statusText = text
}

// statusAnswerText is the answer of the code for this turn, "" when the model answers.
func (t *turnState) statusAnswerText() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.statusText
}
