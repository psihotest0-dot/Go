package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
	"go-monolith/internal/userfiles"
	"go-monolith/metrics"
)

// A task over the files of the person's folder is walked by the server (2026-11-28). "За всеки от
// файловете в папката си направи кратко резюме в нов отделен файл" was left to the model's own plan:
// in the live check of that day the local model read the two files and proposed ONE file of both,
// again on every "продължи" (four proposals), and marked no step of its plan done. Now the code lists
// the files of the folder (not the files a task already made of them) and proposes; the person's yes
// starts the walk, and the runner writes one file for each (taskrun/folder.go). Only the key facts and
// a summary: the reading of a whole text in parts is for the documents of the library.

// folderWords: the person's own folder, or its files, named as what the work is over.
var folderWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:папк\p{L}*|folder|(?:моите|мойте|своите|твоите)\s+файлове|файловете\s+(?:ти|си|ми|на\s+лили)|бележките\s+(?:ти|си|на\s+лили))(?:[^\p{L}]|$)`)

// folderLister lists the files of a person's folder (DefaultToolRegistry with the file tools).
type folderLister interface {
	folderFiles(userID uuid.UUID) ([]userfiles.Info, error)
}

var errNoFolder = errors.New("the file tools are off")

func (r *DefaultToolRegistry) folderFiles(userID uuid.UUID) ([]userfiles.Info, error) {
	if r.files == nil {
		return nil, errNoFolder
	}
	return r.files.List(userID)
}

// folderTaskAnswer is the answer of the code to a task over the files of the folder: the proposal, or
// why there is none; "" when the message is no such task or the server cannot walk it.
func (o *Orchestrator) folderTaskAnswer(ctx context.Context, userID uuid.UUID, session, message string) string {
	if o.runPlans == nil || !folderWords.MatchString(message) {
		return ""
	}
	kind := taskRunKind(message)
	if kind != taskrun.KindKeyFacts && kind != taskrun.KindSummary {
		return ""
	}
	lister, ok := o.tools.(folderLister)
	if !ok {
		return ""
	}
	files, err := lister.folderFiles(userID)
	if err != nil {
		if !errors.Is(err, errNoFolder) {
			slog.WarnContext(ctx, "could not list the folder for a task; the model answers", "err", err)
		}
		return ""
	}
	var steps []taskplan.Step
	var names []string
	made := 0
	for _, f := range files {
		if taskrun.MadeFile(f.Name) {
			made++
			continue
		}
		names = append(names, f.Name)
		if len(steps) < taskplan.MaxSteps {
			steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: f.Name, Folder: f.Name, Status: taskplan.StepPending})
		}
	}
	if len(steps) == 0 {
		metrics.TaskRuns.WithLabelValues(kind, "no_documents").Inc()
		if made > 0 {
			return "В папката ти няма файлове, от които да направя това: тези, които са там, са направени от предишни такива задачи."
		}
		return "Папката ти е празна — няма файлове, за които да направя това."
	}
	if _, err := o.runPlans.Propose(ctx, userID, session, kind, folderGoal(kind, len(steps)), steps); err != nil {
		slog.WarnContext(ctx, "could not store the proposed task over the folder; the model answers", "err", err)
		return ""
	}
	metrics.TaskRuns.WithLabelValues(kind, "proposed").Inc()
	o.dropOtherProposals(ctx, userID, session, proposalTask)
	return folderProposalText(kind, names, len(steps), made)
}

// folderGoal is the goal of a plan over the folder, the way the report and the status answer name it.
func folderGoal(kind string, n int) string {
	if kind == taskrun.KindSummary {
		return fmt.Sprintf("резюме на всеки от %d файла в папката", n)
	}
	return fmt.Sprintf("ключовите факти на всеки от %d файла в папката", n)
}

// folderProposalText is the question before the start of a task over the folder.
func folderProposalText(kind string, names []string, steps, made int) string {
	noun, suffix := "файл с ключовите факти", "ключови факти"
	if kind == taskrun.KindSummary {
		noun, suffix = "файл с резюме", "резюме"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Мога да направя отделен %s за всеки от тези %d файла в папката ти:\n", noun, steps)
	for i, n := range names {
		if i == steps {
			break
		}
		b.WriteString("• " + n + "\n")
	}
	if len(names) > steps {
		fmt.Fprintf(&b, "(Наведнъж правя до %d; останалите %d — след това.)\n", steps, len(names)-steps)
	}
	if made > 0 {
		fmt.Fprintf(&b, "(Пропускам %d, направени от предишни такива задачи.)\n", made)
	}
	fmt.Fprintf(&b, "\nВсеки нов файл е „<име> - %s.md“ и е писан само от текста на своя файл; числата се проверяват срещу него. "+
		"Нищо в папката не се заменя. Ще отнеме около %s и върви на заден план.\n\nДа започна ли?", suffix, estimate(steps))
	return b.String()
}
