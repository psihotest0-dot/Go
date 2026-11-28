package taskrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/db"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
	"go-monolith/metrics"
)

// A task over the files of the person's own folder (2026-11-28). "За всеки от файловете в папката си
// направи кратко резюме в нов отделен файл" was left to the model's own plan (TASK_PLAN_ENABLED):
// in the live check of that day the local model made the plan, read the two files, and proposed ONE
// file of both, again on every "продължи" (four proposals of the same file), and marked no step done.
// Here each file of the folder is a step (taskplan.Step.Folder): the runner reads its text whole and
// writes its own file from it, as it does for a document of the library.

const (
	// folderPartRunes is the size of a piece of a file's text, as a passage of a document.
	folderPartRunes = 1200
	// shortFolderRunes: a file this short has few facts to give; one checked line is enough.
	shortFolderRunes = 800
)

// folderFrom is how a file of the folder is named as the source of what was written from it.
const folderFrom = "от папката"

// advanceFolder makes the file of one file of the folder and saves the plan.
func (r *Runner) advanceFolder(ctx context.Context, p taskplan.RunPlan, step *taskplan.Step, rec recipe) bool {
	step.Status = taskplan.StepDoing
	if !r.save(ctx, p) {
		return false
	}
	stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	evidence, err := r.workFolder(stepCtx, p, rec, step.Folder)
	cancel()
	if ctx.Err() != nil {
		return false // the backend stops; the step is done again after the restart
	}
	if err != nil {
		step.Status, step.Note = taskplan.StepFailed, err.Error()
		metrics.TaskRunSteps.WithLabelValues(p.Kind, resultFailed).Inc()
		slog.Warn("a step of a task over the folder failed", "plan_id", p.ID, "step", step.ID, "reason", err.Error())
	} else {
		step.Status, step.Evidence = taskplan.StepDone, evidence
		metrics.TaskRunSteps.WithLabelValues(p.Kind, resultDone).Inc()
		slog.Info("a step of a task over the folder is done", "plan_id", p.ID, "step", step.ID)
	}
	return r.save(ctx, p)
}

// workFolder makes the file of one file of the folder from its whole text.
func (r *Runner) workFolder(ctx context.Context, p taskplan.RunPlan, rec recipe, name string) (string, error) {
	text, _, err := r.files.Text(p.UserID, name)
	switch {
	case errors.Is(err, userfiles.ErrNotFound):
		return "", fmt.Errorf("файлът вече го няма в папката")
	case err != nil:
		return "", fmt.Errorf("файлът не се прочете: %s", fileError(err))
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("файлът е празен")
	}
	passages := pickPassages(folderParts(text), nil)
	texts := make([]string, 0, len(passages)+1)
	for _, c := range passages {
		texts = append(texts, c.Text)
	}
	texts = append(texts, name)
	least := minFacts
	if utf8.RuneCountInString(text) < shortFolderRunes {
		least = 1
	}
	return r.writeFrom(ctx, p, rec, name, folderFrom, passages, numbersIn(texts...), least)
}

// folderParts cuts a text into pieces at line ends, as the passages of a document.
func folderParts(text string) []db.DocumentChunk {
	var parts []db.DocumentChunk
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			parts = append(parts, db.DocumentChunk{ChunkIndex: int32(len(parts)), Text: s})
		}
		b.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if b.Len() > 0 && utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line) > folderPartRunes {
			flush()
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return parts
}

// writeFrom asks the model for the file of one source from its passages, checks its numbers against
// them, and writes it; least is how many checked lines it must have. It is the end of work as well.
func (r *Runner) writeFrom(ctx context.Context, p taskplan.RunPlan, rec recipe, docName, from string, passages []db.DocumentChunk, known map[string]bool, least int) (string, error) {
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: rec.instructions},
		{Role: openai.ChatMessageRoleUser, Content: passagesText(docName, passages)},
	}
	answer, err := r.ask(ctx, messages)
	if err != nil {
		return "", err
	}
	// One more try when numbers came from elsewhere: the model is told which.
	if bad := unsupportedNumbers(answer, known); len(bad) > 0 {
		metrics.TaskRunRetries.WithLabelValues(p.Kind).Inc()
		messages = append(messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: answer},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Тези числа ги няма в откъсите: " + strings.Join(bad, ", ") +
				". Напиши всичко наново по същите правила, само с числа, които са в откъсите."})
		if again, err := r.ask(ctx, messages); err == nil {
			answer = again
		}
	}
	c := checkAnswer(answer, known)
	if c.dropped > 0 {
		metrics.TaskRunDroppedLines.WithLabelValues(p.Kind).Add(float64(c.dropped))
	}
	if c.facts < least {
		return "", fmt.Errorf("от откъсите не излязоха достатъчно проверени факти (%d)", c.facts)
	}
	name, info, err := r.write(p.UserID, docName, from, rec, c, r.now())
	if err != nil {
		return "", err
	}
	evidence := fmt.Sprintf("%s (%d факта, %d байта)", name, c.facts, info.Size)
	if c.dropped > 0 {
		evidence += fmt.Sprintf("; махнати %d реда с числа извън документа", c.dropped)
	}
	return evidence, nil
}

// MadeFile tells whether a file of the folder is one a task of the server wrote ("<x> - резюме.md"):
// a task over the folder does not summarise its own summaries.
func MadeFile(name string) bool {
	stem := docStem(name)
	for _, rec := range recipes {
		if strings.HasSuffix(stem, " - "+rec.fileSuffix) {
			return true
		}
	}
	return false
}
