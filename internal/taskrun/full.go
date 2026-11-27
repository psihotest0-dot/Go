package taskrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/db"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
	"go-monolith/metrics"
)

// A whole document, read in parts (the owner, 2026-11-24, item 5 of the open list): "for each added
// source write detailed notes of ALL of it". The recipes of runner.go give the model the start of
// a document and the passages nearest to a question - 14 000 characters of a paper of 200 000. On
// 2026-11-21 the owner asked for the extraction of the whole articles, part by part, "and then just
// append to the file", and the model wrote "the extraction" from its own memory of the
// conversation, having read nothing (the replay of 2026-11-24 did the same with the button of the
// files: the road was right, the text was not the source's).
//
// Here the server reads the document itself, from its first passage to its last, in parts of about
// PartRunes characters cut at the end of a paragraph or a sentence. For each part the model gets
// that part only, in a request with no tools, and writes the notes of it; every number of the notes
// must be in the part (one retry, then the line is left out), and the notes go to the end of one
// file under the heading of the part. A part that is only a table of contents, a list of references
// or a licence gets "НЯМА" and no section. One part is one unit of work of the runner: the plan
// is saved after each, so a stop takes effect between two parts and a restart goes on with the
// next one.

// KindFull is a file with detailed notes of the whole of each document, read in parts.
const KindFull = "full"

const (
	// PartRunes is the size of a part: about 3 000 tokens of English, with the instructions and
	// the answer well inside a slot of the local model.
	PartRunes = 10000
	// MaxParts bounds a document that is read whole: about 600 000 characters (a long book is not
	// read this way; its key facts are).
	MaxParts = 60
	// minNotes: a part with fewer lines of notes, and not "НЯМА", gets no section and counts as
	// one that did not work.
	minNotes = 2
	// fullAnswerTokens bounds the notes of one part.
	fullAnswerTokens = 1600
	// SecondsPerPart is the time of one part on the local model, for the estimate (a part is about
	// a third of the passages of a recipe).
	SecondsPerPart = 15
	// noneWord is the answer for a part with nothing to note.
	noneWord = "НЯМА"
)

func init() {
	recipes[KindFull] = recipe{
		fileSuffix: "подробни бележки",
		title:      "Подробни бележки",
		noun:       "файл с подробни бележки по целия документ",
		instructions: `Пишеш подробни бележки по ЕДНА ЧАСТ от документ от библиотеката на потребителя. Имаш само текста на тази част — нищо друго.
Правила:
- Напиши от 4 до 12 реда на български, всеки започва с "- ": какво казва частта — твърдения, методи, резултати, примери, определения.
- Всичко е взето от текста на частта. Не добавяй нищо от своите знания, нищо от другите части и нищо, което го няма в текста.
- Всяко число (проценти, години, размери, резултати) препиши точно както е в текста. Ако не си сигурен в някое число, не го пиши.
- Без увод, без заглавие и без заключение: само редовете.
- Ако частта е само съдържание, списък с литература, индекс, лиценз или празни страници, отговори само с думата НЯМА.`,
	}
	Kinds = append(Kinds, KindFull)
}

// documentText is the text of a document as the ingest cut it, put together again: its passages of
// raw text in their order, the overlap of two neighbours (the ingest repeats up to 100 characters)
// taken out. The summaries of concepts are not the document's text and are left out.
func documentText(chunks []db.DocumentChunk) string {
	var b strings.Builder
	prev := ""
	for _, c := range chunks {
		if c.ChunkType != "raw_text" {
			continue
		}
		text := c.Text
		if k := overlap(prev, text); k > 0 {
			text = text[k:]
		}
		b.WriteString(text)
		prev = c.Text
	}
	return b.String()
}

// overlap is the length in bytes of the longest end of a that b begins with, up to 160 runes and
// from 20 bytes on (a shorter one is a coincidence).
func overlap(a, b string) int {
	const most, least = 160 * 4, 20
	for k := min(len(a), len(b), most); k >= least; k-- {
		if k < len(b) && !utf8.RuneStart(b[k]) {
			continue // b[:k] would end inside a letter
		}
		if strings.HasSuffix(a, b[:k]) {
			return k
		}
	}
	return 0
}

// splitParts cuts text into parts of about size runes: a part ends at the last paragraph break in
// its second half, else at the last end of a sentence, else at a space, else where it must.
func splitParts(text string, size int) []string {
	text = strings.TrimSpace(text)
	var parts []string
	for text != "" {
		runes := []rune(text)
		if len(runes) <= size+size/5 {
			parts = append(parts, text)
			break
		}
		window := string(runes[:size])
		cut := -1
		half := len(string(runes[:size/2]))
		for _, sep := range []string{"\n\n", ". ", ".\n", "? ", "! ", "\n", " "} {
			if i := strings.LastIndex(window, sep); i >= half {
				cut = i + len(sep)
				break
			}
		}
		if cut < 0 {
			cut = len(window)
		}
		parts = append(parts, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	return parts
}

// fullPart reads the next part of the document of a step and writes its notes; an error is the
// reason the step cannot go on (what was written so far stays in its file).
func (r *Runner) fullPart(ctx context.Context, p taskplan.RunPlan, step *taskplan.Step, file db.IngestedFile) error {
	chunks, err := r.db.Queries.GetDocumentChunksByFile(ctx, db.GetDocumentChunksByFileParams{FileID: file.ID, UserID: p.UserID})
	if err != nil {
		return fmt.Errorf("текстът на документа не се прочете")
	}
	parts := splitParts(documentText(chunks), PartRunes)
	if len(parts) == 0 {
		return fmt.Errorf("документът няма текст в библиотеката")
	}
	if len(parts) > MaxParts {
		return fmt.Errorf("документът е твърде дълъг за четене изцяло (%d части по ~%d знака); за него поискай ключовите факти", len(parts), PartRunes)
	}
	if step.Parts == 0 {
		step.Parts = len(parts)
	}
	k := step.PartsDone // the part to read now, from 0
	if k >= len(parts) {
		k = len(parts) - 1
	}
	notes, why := r.partNotes(ctx, p, file.FileName, parts[k], k+1, len(parts))
	switch {
	case why == noneWord:
		metrics.TaskRunSteps.WithLabelValues(p.Kind, "part_empty").Inc()
	case why != "":
		step.Note = appendNote(step.Note, fmt.Sprintf("част %d: %s", k+1, why))
		metrics.TaskRunSteps.WithLabelValues(p.Kind, "part_failed").Inc()
	default:
		if err := r.ensureFull(p.UserID, file.FileName, step, len(parts)); err != nil {
			return err
		}
		section := fmt.Sprintf("\n## Част %d от %d\n\n%s\n", k+1, len(parts), notes)
		if err := r.appendFull(p.UserID, file.FileName, step, section); err != nil {
			return err
		}
		metrics.TaskRunSteps.WithLabelValues(p.Kind, "part_done").Inc()
	}
	step.PartsDone = k + 1
	// The last part, and no notes in a file yet (every part had nothing, or failed): the file is
	// made all the same, so that the step names it. Not when the backend stops: the part is read
	// again after the restart.
	if step.PartsDone >= len(parts) && ctx.Err() == nil {
		return r.ensureFull(p.UserID, file.FileName, step, len(parts))
	}
	return nil
}

// ensureFull makes the file of the step when it has none. It is made when the first notes are
// ready, not before the first part is read (the code review of 2026-11-27): a restart while the
// first part was read left an empty file, and the part read again after it made a second one.
func (r *Runner) ensureFull(userID uuid.UUID, docName string, step *taskplan.Step, parts int) error {
	if step.File != "" {
		return nil
	}
	name, err := r.createFull(userID, docName, parts)
	if err != nil {
		return err
	}
	step.File = name
	return nil
}

// partNotes are the checked notes of one part; why is noneWord for a part with nothing to note, or
// the reason it did not work.
func (r *Runner) partNotes(ctx context.Context, p taskplan.RunPlan, docName, part string, n, total int) (string, string) {
	rec := recipes[KindFull]
	known := numbersIn(part, docName, fmt.Sprint(n), fmt.Sprint(total))
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: rec.instructions},
		{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("Документ: %s\nЧаст %d от %d:\n\n%s", docName, n, total, part)},
	}
	answer, err := r.askWith(ctx, messages, fullAnswerTokens)
	if err != nil {
		return "", err.Error()
	}
	if isNone(answer) {
		return "", noneWord
	}
	if bad := unsupportedNumbers(answer, known); len(bad) > 0 {
		metrics.TaskRunRetries.WithLabelValues(p.Kind).Inc()
		messages = append(messages,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: answer},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Тези числа ги няма в текста на частта: " + strings.Join(bad, ", ") +
				". Напиши всичко наново по същите правила, само с числа, които са в текста."})
		if again, err := r.askWith(ctx, messages, fullAnswerTokens); err == nil {
			answer = again
		}
	}
	c := checkAnswer(answer, known)
	if c.dropped > 0 {
		metrics.TaskRunDroppedLines.WithLabelValues(p.Kind).Add(float64(c.dropped))
	}
	if c.facts < minNotes {
		return "", "не излязоха проверени бележки"
	}
	return c.text, ""
}

// isNone tells an answer that says the part has nothing to note.
func isNone(answer string) bool {
	a := strings.Trim(strings.TrimSpace(answer), ".!\"„“ ")
	return strings.EqualFold(a, noneWord) || strings.EqualFold(a, "няма")
}

// createFull makes the file of the notes of a document, with its heading; a name that is taken
// gets a number (a file of the folder is never replaced).
func (r *Runner) createFull(userID uuid.UUID, docName string, parts int) (string, error) {
	rec := recipes[KindFull]
	head := fmt.Sprintf("# %s: %s\n\nИзточник: %s (от библиотеката), прочетен целият, на %d части.\nЗапочнато на %s. Бележките на всяка част са писани само от нейния текст; числата са проверени срещу него.\n",
		rec.title, docStem(docName), docName, parts, r.now().Local().Format("02.01.2006 15:04"))
	return r.newFile(userID, docName, rec.fileSuffix, head)
}

// appendFull adds a section to the file of the step; when the file is full (a file of the folder
// holds 64 KB), the notes go on in a new file, which becomes the step's.
func (r *Runner) appendFull(userID uuid.UUID, docName string, step *taskplan.Step, section string) error {
	_, err := r.files.Append(userID, step.File, section)
	if err == nil {
		return nil
	}
	var invalid *userfiles.InvalidError
	if !errors.As(err, &invalid) && !errors.Is(err, userfiles.ErrNotFound) {
		slog.Warn("the notes of a part could not be added", "error", err)
		return fmt.Errorf("бележките не се добавиха във файла %s: %s", step.File, fileError(err))
	}
	// Full, or the person deleted the file meanwhile: the notes go on in a new one.
	head := fmt.Sprintf("# %s: %s (продължение)\n\nИзточник: %s (от библиотеката).\n", recipes[KindFull].title, docStem(docName), docName)
	name, err := r.newFile(userID, docName, recipes[KindFull].fileSuffix+" - продължение", head)
	if err != nil {
		return err
	}
	step.Evidence = appendNote(step.Evidence, step.File)
	step.File = name
	if _, err := r.files.Append(userID, name, section); err != nil {
		return fmt.Errorf("бележките не се добавиха във файла %s: %s", name, fileError(err))
	}
	return nil
}

// newFile writes a new file named after the document and suffix; a name that is taken gets a number.
func (r *Runner) newFile(userID uuid.UUID, docName, suffix, content string) (string, error) {
	base := FileName(docName, suffix, 0)
	for n := 0; n < 10; n++ {
		name := FileName(docName, suffix, n)
		_, err := r.files.Create(userID, name, content)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, userfiles.ErrExists) {
			slog.Warn("a file of a task could not be written", "error", err)
			return "", fmt.Errorf("файлът %s не се записа: %s", base, fileError(err))
		}
	}
	return "", fmt.Errorf("има вече 10 файла с името %s", base)
}

// fullEvidence is the line of a whole document that was read: its files and its parts.
func fullEvidence(step *taskplan.Step) string {
	files := step.File
	if step.Evidence != "" {
		files = step.Evidence + ", " + step.File
	}
	line := fmt.Sprintf("%s (прочетен целият: %d части)", files, step.Parts)
	if step.Note != "" {
		line += "; не станаха: " + step.Note
	}
	return line
}

func appendNote(note, more string) string {
	if note == "" {
		return more
	}
	return note + "; " + more
}
