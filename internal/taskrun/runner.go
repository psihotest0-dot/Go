// Package taskrun walks the plans of tasks that the server makes itself (plan steps 3.3-3.7,
// 2026-11-23): "for each source make a file with its key facts".
//
// Why the server and not the model. On 2026-11-21 the real model was given that request over 13
// sources in its library, with a plan kept by the server in front of it in every turn: it made the
// plan and followed it, and wrote 5 files, none of them from its source (6 of 7 invented without
// the plan). A 12B model does not orchestrate 13 sources with two tools each; it writes one short
// text well when the text it is to write from is in front of it. So here the code does the rest:
//
//   - the steps are the documents, made by the code (one step for each), and the person says yes
//     once before anything starts (the owner, 2026-11-23: that yes is enough for every file of the
//     list, and a file of the folder is never replaced);
//   - for each document the server reads its passages (its start, and the passages nearest to the
//     recipe's question), and the model gets only them, in one request with no tools;
//   - every number of the answer must be in the passages: a line with a number from elsewhere is
//     left out, and an answer with fewer than three facts left is a failed step with its reason;
//   - the file is written by the server as a new file, and the step is done with the file's name
//     as its evidence; at the end the server writes the report in the conversation.
//
// The runner is one goroutine of the backend, not a job of River: a job that was running when the
// backend stopped is rescued by River only after an hour, and the state of a walk is all in its
// plan (task_plans), so after a restart the runner reads the plans that are active and goes on
// with their next steps.
package taskrun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
	"go-monolith/metrics"
)

const (
	// pollEvery is how often the runner looks at the plans when nothing woke it: a document that is
	// still being processed by the ingest may be ready by then.
	pollEvery = 30 * time.Second
	// waitForIngest is how long a step waits for its document to be processed, from the proposal.
	waitForIngest = 45 * time.Minute
	// stepTimeout bounds the work on one document (the model may wait for a free slot).
	stepTimeout = 6 * time.Minute
	// nearPassages is how many passages the search of the recipe's question brings.
	nearPassages = 8
	// SecondsPerItem is the time of one document on the local model, for the estimate the person
	// is told before the start (about 30 s measured on 2026-11-23: the passages and the answer).
	SecondsPerItem = 30
)

// Results of a step, the labels of the metric task_run_steps_total.
const (
	resultDone    = "done"
	resultFailed  = "failed"
	resultStopped = "stopped"
)

// Searcher finds the passages of one document nearest to a query (retrieval.RetrievalService).
type Searcher interface {
	SearchFileChunks(ctx context.Context, userID, fileID uuid.UUID, query string, limit int) ([]retrieval.SearchResult, error)
}

// Files is the folder the files are written to (userfiles.Store).
type Files interface {
	Create(userID uuid.UUID, name, content string) (userfiles.Info, error)
	Append(userID uuid.UUID, name, text string) (userfiles.Info, error)
	// Text is the whole text of a file of the folder (a task over the folder, folder.go).
	Text(userID uuid.UUID, name string) (string, userfiles.Info, error)
}

// LinkAdder puts the document behind a link the person wrote into the library (the road of
// add_to_library: agent.DefaultToolRegistry). The error's text is the reason in the person's words.
type LinkAdder interface {
	AddLink(ctx context.Context, userID uuid.UUID, link string) (id uuid.UUID, name string, err error)
}

// Notifier tells the person something outside the conversation's own history (Telegram); nil: nothing.
type Notifier func(ctx context.Context, userID uuid.UUID, session, text string)

// Runner walks the plans; see the package comment.
type Runner struct {
	db     *db.Database
	plans  *taskplan.Store
	search Searcher
	llm    llm.LLMClient
	files  Files
	links  LinkAdder
	notify Notifier
	wake   chan struct{}
	now    func() time.Time
}

// New returns a runner; Run starts it.
func New(database *db.Database, plans *taskplan.Store, search Searcher, client llm.LLMClient, files Files) *Runner {
	return &Runner{db: database, plans: plans, search: search, llm: client, files: files,
		wake: make(chan struct{}, 1), now: time.Now}
}

// WithLinks lets the runner walk tasks over links: each is put into the library first.
func (r *Runner) WithLinks(a LinkAdder) *Runner {
	r.links = a
	return r
}

// WithNotifier sends the report of a finished walk to n as well.
func (r *Runner) WithNotifier(n Notifier) *Runner {
	r.notify = n
	return r
}

// Wake makes the runner look at the plans now (a plan was started). It never blocks.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run walks the plans until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	slog.Info("the runner of tasks is on")
	for {
		for r.pass(ctx) {
			if ctx.Err() != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(pollEvery):
		}
	}
}

// pass does the next step of every plan that is being walked; true when some step was done, so
// that the next pass comes at once.
func (r *Runner) pass(ctx context.Context) bool {
	running, err := r.plans.AllRunning(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("the runner could not read the plans; it tries again later", "error", err)
		}
		return false
	}
	worked := false
	for _, p := range running {
		if ctx.Err() != nil {
			return false
		}
		if r.advance(ctx, p) {
			worked = true
		}
	}
	return worked
}

// advance does the next step of p whose document is ready, and ends the plan when no step is left.
// true when it did a step or ended the plan.
func (r *Runner) advance(ctx context.Context, p taskplan.RunPlan) bool {
	rec, ok := recipes[p.Kind]
	if !ok {
		slog.Error("a plan of a kind the runner does not know; it is stopped", "plan_id", p.ID, "kind", p.Kind)
		p.Status = taskplan.PlanCancelled
		_, _ = r.plans.SaveRun(ctx, p.UserID, p.Plan)
		return false
	}
	for i := range p.Steps {
		step := &p.Steps[i]
		if step.Status != taskplan.StepPending && step.Status != taskplan.StepDoing {
			continue
		}
		if step.Ref == "" && step.Link != "" {
			// A link not yet in the library: putting it there is the unit of work of this pass.
			return r.addLink(ctx, p, step)
		}
		if step.Folder != "" {
			// A file of the person's folder: read whole, no document to wait for (folder.go).
			return r.advanceFolder(ctx, p, step, rec)
		}
		file, ready, why := r.document(ctx, p, step)
		if why != "" {
			step.Status, step.Note = taskplan.StepFailed, why
			metrics.TaskRunSteps.WithLabelValues(p.Kind, resultFailed).Inc()
			return r.save(ctx, p)
		}
		if !ready {
			continue // its document is still being processed; the others go first
		}
		step.Status = taskplan.StepDoing
		if !r.save(ctx, p) {
			return false
		}
		if p.Kind == KindFull {
			return r.advanceFull(ctx, p, step, file)
		}
		stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
		started := r.now()
		evidence, err := r.work(stepCtx, p, rec, file)
		cancel()
		if ctx.Err() != nil {
			return false // the backend stops; the step is done again after the restart
		}
		if err != nil {
			step.Status, step.Note = taskplan.StepFailed, err.Error()
			metrics.TaskRunSteps.WithLabelValues(p.Kind, resultFailed).Inc()
			slog.Warn("a step of a task failed", "plan_id", p.ID, "step", step.ID, "reason", err.Error())
		} else {
			step.Status, step.Evidence = taskplan.StepDone, evidence
			metrics.TaskRunSteps.WithLabelValues(p.Kind, resultDone).Inc()
			slog.Info("a step of a task is done", "plan_id", p.ID, "step", step.ID, "seconds", int(r.now().Sub(started).Seconds()))
		}
		return r.save(ctx, p)
	}
	if p.Next() != nil {
		return false // only steps that wait for their documents are left
	}
	p.Status = taskplan.PlanDone
	if !r.save(ctx, p) {
		return false
	}
	r.report(ctx, p)
	return true
}

// advanceFull reads one part of a whole document (full.go) and saves the plan: one part is one
// unit of work, so a stop or a restart comes between two parts.
func (r *Runner) advanceFull(ctx context.Context, p taskplan.RunPlan, step *taskplan.Step, file db.IngestedFile) bool {
	partCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	started := r.now()
	err := r.fullPart(partCtx, p, step, file)
	cancel()
	if ctx.Err() != nil {
		return false // the backend stops; the part is read again after the restart
	}
	switch {
	case err != nil:
		why := err.Error()
		if step.File != "" {
			why += fmt.Sprintf(" (прочетени %d от %d части; записаното е във %s)", step.PartsDone, step.Parts, step.File)
		}
		step.Status, step.Note = taskplan.StepFailed, why
		metrics.TaskRunSteps.WithLabelValues(p.Kind, resultFailed).Inc()
		slog.Warn("a step of a task failed", "plan_id", p.ID, "step", step.ID, "reason", why)
	case step.PartsDone >= step.Parts:
		step.Status, step.Evidence = taskplan.StepDone, fullEvidence(step)
		metrics.TaskRunSteps.WithLabelValues(p.Kind, resultDone).Inc()
		slog.Info("a whole document of a task is read", "plan_id", p.ID, "step", step.ID, "parts", step.Parts)
	default:
		slog.Info("a part of a document of a task is read", "plan_id", p.ID, "step", step.ID,
			"part", step.PartsDone, "parts", step.Parts, "seconds", int(r.now().Sub(started).Seconds()))
	}
	return r.save(ctx, p)
}

// save writes the plan; false when the person stopped it meanwhile (or the write failed).
func (r *Runner) save(ctx context.Context, p taskplan.RunPlan) bool {
	ok, err := r.plans.SaveRun(ctx, p.UserID, p.Plan)
	if err != nil {
		slog.Error("the runner could not save a plan", "plan_id", p.ID, "error", err)
		return false
	}
	if !ok {
		metrics.TaskRunSteps.WithLabelValues(p.Kind, resultStopped).Inc()
		slog.Info("a task was stopped by the person", "plan_id", p.ID)
	}
	return ok
}

// document is the document of a step: ready when it is processed; why is the reason the step
// fails without being tried (the document is gone, its processing failed or takes too long).
func (r *Runner) document(ctx context.Context, p taskplan.RunPlan, step *taskplan.Step) (file db.IngestedFile, ready bool, why string) {
	id, err := uuid.Parse(step.Ref)
	if err != nil {
		return file, false, "стъпката не сочи документ"
	}
	file, err = r.db.Queries.GetIngestedFileByID(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && (file.UserID != p.UserID || file.DeletedAt.Valid):
		return file, false, "документът вече го няма в библиотеката"
	case err != nil:
		slog.Warn("the runner could not read a document; it tries again later", "plan_id", p.ID, "error", err)
		return file, false, ""
	}
	switch file.Status {
	case "completed":
		return file, true, ""
	case "failed":
		return file, false, "документът не можа да се обработи (не се чете текстът му)"
	}
	since := p.CreatedAt
	if step.Added != nil {
		since = *step.Added // a link put into the library by the walk itself
	}
	if r.now().Sub(since) > waitForIngest {
		return file, false, "документът не се обработи навреме"
	}
	return file, false, ""
}

// addLink puts the link of a step into the library, and the step then waits for its processing
// like any document; a link that gives nothing fails the step with its reason.
func (r *Runner) addLink(ctx context.Context, p taskplan.RunPlan, step *taskplan.Step) bool {
	if r.links == nil {
		step.Status, step.Note = taskplan.StepFailed, "свалянето на линкове е изключено"
		metrics.TaskRunSteps.WithLabelValues(p.Kind, resultFailed).Inc()
		return r.save(ctx, p)
	}
	id, name, err := r.links.AddLink(ctx, p.UserID, step.Link)
	if ctx.Err() != nil {
		return false // the backend stops; the link is fetched again after the restart
	}
	if err != nil {
		step.Status, step.Note = taskplan.StepFailed, err.Error()
		metrics.TaskRunSteps.WithLabelValues(p.Kind, resultFailed).Inc()
		slog.Info("a link of a task gave nothing", "plan_id", p.ID, "step", step.ID)
		return r.save(ctx, p)
	}
	at := r.now()
	step.Ref, step.Added = id.String(), &at
	if name != "" {
		step.Text = name
	}
	slog.Info("a link of a task is in the library", "plan_id", p.ID, "step", step.ID)
	return r.save(ctx, p)
}

// work makes the file of one document and returns the line that is its evidence.
func (r *Runner) work(ctx context.Context, p taskplan.RunPlan, rec recipe, file db.IngestedFile) (string, error) {
	chunks, err := r.db.Queries.GetDocumentChunksByFile(ctx, db.GetDocumentChunksByFileParams{FileID: file.ID, UserID: p.UserID})
	if err != nil {
		return "", fmt.Errorf("откъсите на документа не се прочетоха")
	}
	if len(chunks) == 0 {
		return "", fmt.Errorf("документът няма текст в библиотеката")
	}
	var near []string
	if r.search != nil {
		found, err := r.search.SearchFileChunks(ctx, p.UserID, file.ID, rec.query, nearPassages)
		if err != nil {
			slog.Warn("the search inside a document failed; its start and end are used", "plan_id", p.ID, "error", err)
		}
		for _, f := range found {
			near = append(near, f.ID.String())
		}
	}
	passages := pickPassages(chunks, near)
	texts := make([]string, 0, len(passages)+1)
	for _, c := range passages {
		texts = append(texts, c.Text)
	}
	texts = append(texts, file.FileName)
	return r.writeFrom(ctx, p, rec, file.FileName, libraryFrom, passages, numbersIn(texts...), minFacts)
}

// libraryFrom is how a document of the library is named as the source of what was written from it.
const libraryFrom = "от библиотеката"

// ask is one request to the model, without tools and without thinking.
func (r *Runner) ask(ctx context.Context, messages []openai.ChatCompletionMessage) (string, error) {
	return r.askWith(ctx, messages, answerTokens)
}

// askWith is ask with a bound of the answer of its own.
func (r *Runner) askWith(ctx context.Context, messages []openai.ChatCompletionMessage, maxTokens int) (string, error) {
	resp, err := r.llm.Chat(llm.WithBackground(ctx), openai.ChatCompletionRequest{
		Messages: messages,
		//lint:ignore SA1019 llama-server reads max_tokens; max_completion_tokens is OpenAI's own name.
		MaxTokens: maxTokens,
		// A rewrite of the passages: thinking would spend the answer on reasoning.
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
		Temperature:        0.3,
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("моделът не отговори навреме")
		}
		slog.Warn("the model failed on a step of a task", "error", err)
		return "", fmt.Errorf("моделът не отговори")
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("моделът не отговори")
	}
	answer := cleanAnswer(resp.Choices[0].Message.Content)
	if answer == "" {
		return "", fmt.Errorf("моделът върна празен отговор")
	}
	return answer, nil
}

// write writes the file of a document as a new file: a name that is taken gets a number.
func (r *Runner) write(userID uuid.UUID, docName, from string, rec recipe, c checked, at time.Time) (string, userfiles.Info, error) {
	content := fmt.Sprintf("# %s: %s\n\nИзточник: %s (%s)\nНаписано на %s от текста на източника; числата са проверени срещу него.\n\n%s\n",
		rec.title, docStem(docName), docName, from, at.Local().Format("02.01.2006 15:04"), c.text)
	base := FileName(docName, rec.fileSuffix, 0)
	for n := 0; n < 10; n++ {
		name := FileName(docName, rec.fileSuffix, n)
		info, err := r.files.Create(userID, name, content)
		if err == nil {
			return name, info, nil
		}
		if !errors.Is(err, userfiles.ErrExists) {
			slog.Warn("a file of a task could not be written", "error", err)
			return "", info, fmt.Errorf("файлът %s не се записа: %s", base, fileError(err))
		}
	}
	return "", userfiles.Info{}, fmt.Errorf("има вече 10 файла с името %s", base)
}

// fileError is why the folder refused a file, in the person's words.
func fileError(err error) string {
	switch {
	case errors.Is(err, userfiles.ErrTooManyFiles):
		return "папката е пълна (твърде много файлове)"
	case errors.Is(err, userfiles.ErrQuotaExceeded):
		return "няма място в папката"
	}
	var invalid *userfiles.InvalidError
	if errors.As(err, &invalid) {
		return "името не става"
	}
	return "грешка при записа"
}

// docStem is the name of a document without its extension.
func docStem(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// FileName is the name of the file made for a document: its name, cleaned to what the folder
// takes, then the suffix of the recipe, then " (n+1)" for n > 0. Always a valid name.
func FileName(docName, suffix string, n int) string {
	var b strings.Builder
	space := false
	for _, r := range docStem(docName) {
		switch {
		case r == '-' || r == '_' || r == '(' || r == ')' || (r >= '0' && r <= '9') || unicode.IsLetter(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		default:
			space = true
		}
	}
	stem := strings.Trim(b.String(), " -_()")
	tail := " - " + suffix
	if n > 0 {
		tail += fmt.Sprintf(" (%d)", n+1)
	}
	tail += ".md"
	room := userfiles.MaxNameRunes - utf8.RuneCountInString(tail)
	if utf8.RuneCountInString(stem) > room {
		stem = strings.TrimRight(string([]rune(stem)[:room]), " -_(")
	}
	if stem == "" {
		stem = "документ"
	}
	name := stem + tail
	if userfiles.ValidateName(name) != nil {
		name = "документ" + tail
	}
	return name
}

// ReportLog starts the record (messages.tool_log) of the report of a walk in its conversation: a
// message the server writes on its own, which a "да" after it does not answer
// (agent.yesAfterServerNotice).
const ReportLog = "task_run: "

// report writes in the conversation what the walk did, and sends it to the notifier.
func (r *Runner) report(ctx context.Context, p taskplan.RunPlan) {
	text := ReportText(p.Plan)
	if _, err := r.db.Queries.CreateMessage(ctx, db.CreateMessageParams{
		UserID: p.UserID, SessionID: p.Session, Role: "assistant", Content: text,
		SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true},
		ToolLog:        sql.NullString{String: ReportLog + reportLog(p.Plan), Valid: true},
	}); err != nil {
		slog.Error("the report of a task could not be written in the conversation", "plan_id", p.ID, "error", err)
	}
	metrics.TaskRuns.WithLabelValues(p.Kind, "done").Inc()
	if r.notify != nil {
		r.notify(ctx, p.UserID, p.Session, text)
	}
}

// ReportText is the report of a walk that ended, in the person's words.
func ReportText(p *taskplan.Plan) string {
	var done, failed []taskplan.Step
	for _, s := range p.Steps {
		switch s.Status {
		case taskplan.StepDone:
			done = append(done, s)
		case taskplan.StepFailed, taskplan.StepSkipped:
			failed = append(failed, s)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Готово: %s — %d от %d.", p.Goal, len(done), len(p.Steps))
	if len(done) > 0 {
		b.WriteString("\n\nЗаписани файлове:")
		for _, s := range done {
			b.WriteString("\n• " + s.Evidence)
		}
	}
	if len(failed) > 0 {
		b.WriteString("\n\nНе станаха:")
		for _, s := range failed {
			b.WriteString("\n• " + s.Text + " — " + s.Note)
		}
	}
	b.WriteString("\n\nФайловете са в папката; всяко число в тях е проверено срещу откъсите на документа.")
	return b.String()
}

// reportLog is the line of the tool record of the report (what the model reads in the history).
func reportLog(p *taskplan.Plan) string {
	done := 0
	for _, s := range p.Steps {
		if s.Status == taskplan.StepDone {
			done++
		}
	}
	return fmt.Sprintf("%d of %d files written by the server", done, len(p.Steps))
}
