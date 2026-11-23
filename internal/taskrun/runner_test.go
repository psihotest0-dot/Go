package taskrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
)

// scriptedLLM answers the n-th request with the n-th answer (the last one again after them), and
// keeps the requests.
type scriptedLLM struct {
	llm.MockLLM
	mu       sync.Mutex
	answers  []string
	requests []openai.ChatCompletionRequest
	err      error
}

func (s *scriptedLLM) Chat(_ context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.err != nil {
		return openai.ChatCompletionResponse{}, s.err
	}
	answer := s.answers[min(len(s.requests), len(s.answers))-1]
	return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: answer}}}}, nil
}

type env struct {
	ctx    context.Context
	db     *db.Database
	plans  *taskplan.Store
	files  *userfiles.Store
	dir    string
	user   uuid.UUID
	llm    *scriptedLLM
	runner *Runner
}

func newEnv(t *testing.T, answers ...string) *env {
	t.Helper()
	conn := dbtest.ConnString(t)
	t.Chdir("../..") // the migrations are found relative to the working directory
	require.NoError(t, db.RunMigrations(conn))
	ctx := context.Background()
	database, err := db.Connect(ctx, conn)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })
	dir := t.TempDir()
	files, err := userfiles.Open(dir, userfiles.Limits{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = files.Close() })
	plans := taskplan.NewStore(database)
	fake := &scriptedLLM{answers: answers}
	return &env{ctx: ctx, db: database, plans: plans, files: files, dir: filepath.Join(dir, user.ID.String()), user: user.ID, llm: fake,
		runner: New(database, plans, nil, fake, files)}
}

// document puts a document with the given passages in the library; status is its ingest status.
func (e *env) document(t *testing.T, name, status string, texts ...string) db.IngestedFile {
	t.Helper()
	file, err := e.db.Queries.CreateIngestedFile(e.ctx, db.CreateIngestedFileParams{
		UserID: e.user, FileName: name, FileHash: "hash-" + name + uuid.NewString(),
		MimeType: "application/pdf", FileSize: 1, Status: status, LocalPath: "storage/none",
	})
	require.NoError(t, err)
	if len(texts) == 0 {
		return file
	}
	vectors, err := (&llm.MockEmbedder{}).EmbedTexts(e.ctx, texts, false)
	require.NoError(t, err)
	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i, text := range texts {
		arg.FileIds = append(arg.FileIds, file.ID)
		arg.UserIds = append(arg.UserIds, e.user)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, text)
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(vectors[i]))
		arg.ChunkTypes = append(arg.ChunkTypes, "raw_text")
		arg.Metadatas = append(arg.Metadatas, "{}")
	}
	require.NoError(t, e.db.Queries.CreateDocumentChunksBatch(e.ctx, arg))
	return file
}

// start proposes a plan over the documents and starts it, as the yes of the person does.
func (e *env) start(t *testing.T, kind, session string, docs ...db.IngestedFile) *taskplan.Plan {
	t.Helper()
	var steps []taskplan.Step
	for i, d := range docs {
		steps = append(steps, taskplan.Step{ID: i + 1, Text: d.FileName, Ref: d.ID.String(), Status: taskplan.StepPending})
	}
	proposed, err := e.plans.Propose(e.ctx, e.user, session, kind, "ключовите факти на всеки документ", steps)
	require.NoError(t, err)
	started, err := e.plans.Start(e.ctx, e.user, proposed.ID)
	require.NoError(t, err)
	return started
}

// walk runs passes until nothing more is done.
func (e *env) walk(t *testing.T) {
	t.Helper()
	for i := 0; i < 50 && e.runner.pass(e.ctx); i++ {
	}
}

func (e *env) plan(t *testing.T, session string) *taskplan.Plan {
	t.Helper()
	p, err := e.plans.LatestRun(e.ctx, e.user, session)
	require.NoError(t, err)
	require.NotNil(t, p)
	return p
}

func (e *env) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, name))
	require.NoError(t, err)
	return string(raw)
}

func (e *env) assistantMessages(t *testing.T, session string) []string {
	t.Helper()
	rows, err := e.db.SqlDB.QueryContext(e.ctx, `SELECT content FROM messages WHERE user_id = $1 AND session_id = $2 AND role = 'assistant' ORDER BY created_at`, e.user, session)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

const factsAnswer = "- Нощната доходност е 1.41% годишно.\n- t-статистиката е 1.52.\n- Ефектът е най-силен при оборот 1.63%.\n- Разходите от 1.74% го отслабват."

func TestTheRunnerWritesAFileOfCheckedFactsForEachDocument(t *testing.T) {
	e := newEnv(t, factsAnswer)
	a := e.document(t, "overnight-returns.pdf", "completed",
		"The annualised overnight return is 1.41% with a t-statistic of 1.52.",
		"The effect is strongest for stocks with 1.63% daily turnover and weakens after costs of 1.74%.")
	b := e.document(t, "intraday-reversal.pdf", "completed",
		"Returns 1.41 and 1.52 and 1.63 and 1.74 are reported.")
	e.start(t, KindKeyFacts, "s", a, b)

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	for _, s := range p.Steps {
		assert.Equal(t, taskplan.StepDone, s.Status, s.Note)
		assert.Contains(t, s.Evidence, "ключови факти.md")
		assert.Contains(t, s.Evidence, "4 факта")
	}
	text := e.read(t, "overnight-returns - ключови факти.md")
	assert.Contains(t, text, "Източник: overnight-returns.pdf")
	assert.Contains(t, text, "- Нощната доходност е 1.41% годишно.")
	assert.FileExists(t, filepath.Join(e.dir, "intraday-reversal - ключови факти.md"))

	// The model got the passages of the document and nothing else, without tools and thinking.
	require.Len(t, e.llm.requests, 2)
	req := e.llm.requests[0]
	assert.Empty(t, req.Tools)
	assert.Equal(t, map[string]any{"enable_thinking": false}, req.ChatTemplateKwargs)
	assert.Contains(t, req.Messages[1].Content, "Документ: overnight-returns.pdf")
	assert.Contains(t, req.Messages[1].Content, "daily turnover")
	assert.NotContains(t, req.Messages[1].Content, "Returns 1.41 and", "the passages of the other document are not in its request")

	// The report is in the conversation.
	msgs := e.assistantMessages(t, "s")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "2 от 2")
	assert.Contains(t, msgs[0], "overnight-returns - ключови факти.md")
}

func TestANumberFromElsewhereIsAskedAgainAndItsLineLeftOut(t *testing.T) {
	invented := factsAnswer + "\n- Изследването обхваща 37.9% от пазара."
	e := newEnv(t, invented, invented)
	a := e.document(t, "paper.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	e.start(t, KindKeyFacts, "s", a)

	e.walk(t)

	require.Len(t, e.llm.requests, 2, "the model is asked again once")
	retry := e.llm.requests[1].Messages
	assert.Contains(t, retry[len(retry)-1].Content, "37.9", "it is told which number is not in the passages")
	text := e.read(t, "paper - ключови факти.md")
	assert.NotContains(t, text, "37.9", "a line with a number from elsewhere is not written")
	assert.Contains(t, text, "1.41%")
	p := e.plan(t, "s")
	assert.Contains(t, p.Steps[0].Evidence, "махнати 1 реда")
}

func TestTheSecondAnswerIsUsedWhenItMendsTheNumbers(t *testing.T) {
	e := newEnv(t, factsAnswer+"\n- Обхваща 37.9%.", factsAnswer+"\n- Обхваща 1.63% от оборота.")
	a := e.document(t, "paper.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	e.start(t, KindKeyFacts, "s", a)

	e.walk(t)

	text := e.read(t, "paper - ключови факти.md")
	assert.Contains(t, text, "Обхваща 1.63% от оборота.")
	assert.NotContains(t, e.plan(t, "s").Steps[0].Evidence, "махнати")
}

func TestTooFewCheckedFactsFailTheStepWithItsReason(t *testing.T) {
	e := newEnv(t, "- Ефектът е 9.99%.\n- Рискът е 8.88%.\n- Само това.")
	a := e.document(t, "paper.pdf", "completed", "Return 1.41.")
	e.start(t, KindKeyFacts, "s", a)

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Contains(t, p.Steps[0].Note, "не излязоха достатъчно проверени факти")
	entries, _ := os.ReadDir(e.dir)
	assert.Empty(t, entries, "no file is written")
	msgs := e.assistantMessages(t, "s")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "0 от 1")
	assert.Contains(t, msgs[0], "Не станаха:\n• paper.pdf — от откъсите")
}

func TestAFileOfTheFolderIsNeverReplaced(t *testing.T) {
	e := newEnv(t, factsAnswer)
	_, err := e.files.Create(e.user, "paper - ключови факти.md", "моите бележки")
	require.NoError(t, err)
	a := e.document(t, "paper.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	e.start(t, KindKeyFacts, "s", a)

	e.walk(t)

	assert.Equal(t, "моите бележки", e.read(t, "paper - ключови факти.md"))
	assert.Contains(t, e.read(t, "paper - ключови факти (2).md"), "1.41%")
}

func TestAStepWaitsForItsDocumentAndTheOthersGoFirst(t *testing.T) {
	e := newEnv(t, factsAnswer)
	waiting := e.document(t, "later.pdf", "processing")
	ready := e.document(t, "ready.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	e.start(t, KindKeyFacts, "s", waiting, ready)

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanActive, p.Status, "the plan waits for the document")
	assert.Equal(t, taskplan.StepPending, p.Steps[0].Status)
	assert.Equal(t, taskplan.StepDone, p.Steps[1].Status)

	// The ingest finishes: the next pass does it.
	_, err := e.db.SqlDB.ExecContext(e.ctx, `UPDATE ingested_files SET status = 'completed' WHERE id = $1`, waiting.ID)
	require.NoError(t, err)
	_, err = e.db.SqlDB.ExecContext(e.ctx, `INSERT INTO document_chunks (file_id, user_id, chunk_index, text, embedding, chunk_type, metadata)
		SELECT $1, user_id, 0, 'Return 1.41, t 1.52, turnover 1.63, costs 1.74.', embedding, chunk_type, metadata FROM document_chunks WHERE file_id = $2 LIMIT 1`, waiting.ID, ready.ID)
	require.NoError(t, err)
	e.walk(t)

	p = e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status, p.Steps[0].Note)
}

func TestADocumentThatIsNeverProcessedFailsItsStepInTime(t *testing.T) {
	e := newEnv(t, factsAnswer)
	waiting := e.document(t, "later.pdf", "pending")
	e.start(t, KindKeyFacts, "s", waiting)
	e.runner.now = func() time.Time { return time.Now().Add(waitForIngest + time.Minute) }

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Equal(t, "документът не се обработи навреме", p.Steps[0].Note)
}

func TestAGoneOrFailedDocumentFailsItsStep(t *testing.T) {
	e := newEnv(t, factsAnswer)
	failed := e.document(t, "broken.pdf", "failed")
	gone := e.document(t, "gone.pdf", "completed", "x 1.41")
	_, err := e.db.SqlDB.ExecContext(e.ctx, `UPDATE ingested_files SET deleted_at = now(), status = 'deleted' WHERE id = $1`, gone.ID)
	require.NoError(t, err)
	e.start(t, KindKeyFacts, "s", failed, gone)

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Contains(t, p.Steps[0].Note, "не можа да се обработи")
	assert.Equal(t, taskplan.StepFailed, p.Steps[1].Status)
	assert.Equal(t, "документът вече го няма в библиотеката", p.Steps[1].Note)
	assert.Empty(t, e.llm.requests)
}

func TestAStoppedTaskWritesNothingMore(t *testing.T) {
	e := newEnv(t, factsAnswer)
	a := e.document(t, "a.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	e.start(t, KindKeyFacts, "s", a)
	stopped, err := e.plans.StopRun(e.ctx, e.user, "s")
	require.NoError(t, err)
	require.True(t, stopped)

	e.walk(t)

	assert.Empty(t, e.llm.requests)
	assert.Empty(t, e.assistantMessages(t, "s"))
	assert.Equal(t, taskplan.PlanCancelled, e.plan(t, "s").Status)
}

func TestAnotherPersonsDocumentIsNotRead(t *testing.T) {
	e := newEnv(t, factsAnswer)
	other, err := e.db.Queries.CreateUser(e.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.db.Pool, other.ID) })
	theirs, err := e.db.Queries.CreateIngestedFile(e.ctx, db.CreateIngestedFileParams{
		UserID: other.ID, FileName: "theirs.pdf", FileHash: "h-" + uuid.NewString(), MimeType: "application/pdf", FileSize: 1, Status: "completed", LocalPath: "x",
	})
	require.NoError(t, err)
	e.start(t, KindKeyFacts, "s", theirs)

	e.walk(t)

	assert.Equal(t, "документът вече го няма в библиотеката", e.plan(t, "s").Steps[0].Note)
	assert.Empty(t, e.llm.requests)
}

func TestAModelThatFailsFailsTheStepAndTheRestGoOn(t *testing.T) {
	e := newEnv(t, factsAnswer)
	e.llm.err = fmt.Errorf("server 500")
	a := e.document(t, "a.pdf", "completed", "Return 1.41.")
	e.start(t, KindKeyFacts, "s", a)

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Equal(t, "моделът не отговори", p.Steps[0].Note)
}

func TestASummaryHasItsOwnRecipeAndName(t *testing.T) {
	e := newEnv(t, "Документът е за нощната доходност.\n\nОсновни точки:\n- 1.41% годишно.\n- t 1.52.\n- оборот 1.63%.")
	a := e.document(t, "paper.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63.")
	e.start(t, KindSummary, "s", a)

	e.walk(t)

	require.Len(t, e.llm.requests, 1)
	assert.Contains(t, e.llm.requests[0].Messages[0].Content, "резюме")
	text := e.read(t, "paper - резюме.md")
	assert.True(t, strings.HasPrefix(text, "# Резюме: paper"))
	assert.Contains(t, text, "Документът е за нощната доходност.")
}

func TestAfterARestartTheRunnerGoesOnWithTheNextStep(t *testing.T) {
	e := newEnv(t, factsAnswer)
	a := e.document(t, "a.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	b := e.document(t, "b.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	p := e.start(t, KindKeyFacts, "s", a, b)
	// The backend stopped while step 2 was being done; step 1 was finished.
	p.Steps[0].Status, p.Steps[0].Evidence = taskplan.StepDone, "a - ключови факти.md"
	p.Steps[1].Status = taskplan.StepDoing
	ok, err := e.plans.SaveRun(e.ctx, e.user, p)
	require.NoError(t, err)
	require.True(t, ok)

	fresh := New(e.db, e.plans, nil, e.llm, e.files)
	for i := 0; i < 10 && fresh.pass(e.ctx); i++ {
	}

	require.Len(t, e.llm.requests, 1, "only the step that was not finished is done")
	assert.Contains(t, e.llm.requests[0].Messages[1].Content, "Документ: b.pdf")
	assert.Equal(t, taskplan.PlanDone, e.plan(t, "s").Status)
}

func TestRunEndsWithTheContextAndWakesOnStart(t *testing.T) {
	e := newEnv(t, factsAnswer)
	a := e.document(t, "a.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { e.runner.Run(ctx); close(done) }()

	e.start(t, KindKeyFacts, "s", a)
	e.runner.Wake()
	e.runner.Wake() // a second wake does not block
	require.Eventually(t, func() bool {
		p, err := e.plans.LatestRun(e.ctx, e.user, "s")
		return err == nil && p != nil && p.Status == taskplan.PlanDone
	}, 10*time.Second, 50*time.Millisecond, "the wake starts the walk without waiting for the poll")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end with its context")
	}
}

// seeingLLM records what the plan shows while the model writes.
type seeingLLM struct {
	scriptedLLM
	see  func() string
	seen []string
}

func (s *seeingLLM) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	s.seen = append(s.seen, s.see())
	return s.scriptedLLM.Chat(ctx, req)
}

func TestWhileTheModelWritesThePlanShowsTheStepAsBeingDone(t *testing.T) {
	e := newEnv(t, factsAnswer)
	a := e.document(t, "a.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	e.start(t, KindKeyFacts, "s", a)
	seeing := &seeingLLM{scriptedLLM: scriptedLLM{answers: []string{factsAnswer}}}
	seeing.see = func() string {
		p, err := e.plans.LatestRun(e.ctx, e.user, "s")
		require.NoError(t, err)
		return p.Steps[0].Status
	}
	e.runner.llm = seeing

	e.walk(t)

	assert.Equal(t, []string{taskplan.StepDoing}, seeing.seen, "the app and \"докъде стигна?\" see which document is being done")
	assert.Equal(t, taskplan.StepDone, e.plan(t, "s").Steps[0].Status)
}

func TestAPlanStoppedAfterItWasReadDoesNoStep(t *testing.T) {
	e := newEnv(t, factsAnswer)
	a := e.document(t, "a.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	e.start(t, KindKeyFacts, "s", a)
	all, err := e.plans.AllRunning(e.ctx)
	require.NoError(t, err)
	var mine taskplan.RunPlan
	for _, p := range all {
		if p.UserID == e.user {
			mine = p
		}
	}
	require.NotNil(t, mine.Plan)
	// The person says "спри" between the read of the plans and the step.
	_, err = e.plans.StopRun(e.ctx, e.user, "s")
	require.NoError(t, err)

	assert.False(t, e.runner.advance(e.ctx, mine))

	assert.Empty(t, e.llm.requests, "the model is not asked for a stopped plan")
	entries, _ := os.ReadDir(e.dir)
	assert.Empty(t, entries)
}
