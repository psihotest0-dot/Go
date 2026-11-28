package taskrun

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/taskplan"
)

// fakeLinks puts a link into the library as the test says: a document made by the test, or an error.
type fakeLinks struct {
	docs  map[string]db.IngestedFile
	errs  map[string]error
	asked []string
}

func (f *fakeLinks) AddLink(_ context.Context, _ uuid.UUID, link string) (uuid.UUID, string, error) {
	f.asked = append(f.asked, link)
	if err, ok := f.errs[link]; ok {
		return uuid.Nil, "", err
	}
	d := f.docs[link]
	return d.ID, d.FileName, nil
}

// startLinks proposes a plan over links and starts it, as the yes of the person does.
func (e *env) startLinks(t *testing.T, kind, session string, links ...string) *taskplan.Plan {
	t.Helper()
	var steps []taskplan.Step
	for i, l := range links {
		steps = append(steps, taskplan.Step{ID: i + 1, Text: "link " + l, Link: l, Status: taskplan.StepPending})
	}
	proposed, err := e.plans.Propose(e.ctx, e.user, session, kind, "ключовите факти на всеки линк", steps)
	require.NoError(t, err)
	started, err := e.plans.Start(e.ctx, e.user, proposed.ID)
	require.NoError(t, err)
	return started
}

func TestEachLinkIsPutIntoTheLibraryAndThenItsFileIsMade(t *testing.T) {
	e := newEnv(t, factsAnswer)
	doc := e.document(t, "overnight.pdf", "pending")
	links := &fakeLinks{docs: map[string]db.IngestedFile{"https://a.example/p.pdf": doc}}
	e.runner.WithLinks(links)
	e.startLinks(t, KindKeyFacts, "s", "https://a.example/p.pdf")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, []string{"https://a.example/p.pdf"}, links.asked, "fetched once")
	assert.Equal(t, doc.ID.String(), p.Steps[0].Ref, "the step now works on the document")
	assert.Equal(t, "overnight.pdf", p.Steps[0].Text, "named as the library names it")
	require.NotNil(t, p.Steps[0].Added)
	assert.Equal(t, taskplan.StepPending, p.Steps[0].Status, "it waits for its processing")
	assert.Equal(t, taskplan.PlanActive, p.Status)

	// The processing ends: the file is made from its passages.
	_, err := e.db.SqlDB.ExecContext(e.ctx, `UPDATE ingested_files SET status = 'completed' WHERE id = $1`, doc.ID)
	require.NoError(t, err)
	ready := e.document(t, "other.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	_, err = e.db.SqlDB.ExecContext(e.ctx, `UPDATE document_chunks SET file_id = $1 WHERE file_id = $2`, doc.ID, ready.ID)
	require.NoError(t, err)
	e.walk(t)

	p = e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status, p.Steps[0].Note)
	assert.FileExists(t, e.dir+"/overnight - ключови факти.md")
	assert.Len(t, links.asked, 1, "a link in the library is not fetched again")
}

func TestALinkThatGivesNothingFailsItsStepWithTheReasonAndTheRestGoOn(t *testing.T) {
	e := newEnv(t, factsAnswer)
	doc := e.document(t, "ready.pdf", "completed", "Return 1.41, t 1.52, turnover 1.63, costs 1.74.")
	links := &fakeLinks{
		docs: map[string]db.IngestedFile{"https://b.example/ok.pdf": doc},
		errs: map[string]error{"https://a.example/blocked": errors.New("сайтът не пуска програми (отказ 401/403)")},
	}
	e.runner.WithLinks(links)
	e.startLinks(t, KindKeyFacts, "s", "https://a.example/blocked", "https://b.example/ok.pdf")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Equal(t, "сайтът не пуска програми (отказ 401/403)", p.Steps[0].Note)
	assert.Equal(t, taskplan.StepDone, p.Steps[1].Status, p.Steps[1].Note)
	msgs := e.assistantMessages(t, "s")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "1 от 2")
	assert.Contains(t, msgs[0], "link https://a.example/blocked — сайтът не пуска програми")
}

func TestWithoutTheLinkAdderALinkStepFails(t *testing.T) {
	e := newEnv(t, factsAnswer)
	e.startLinks(t, KindKeyFacts, "s", "https://a.example/p.pdf")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Equal(t, "свалянето на линкове е изключено", p.Steps[0].Note)
}

// The wait for the processing of a link counts from when the walk put it into the library, not from
// the proposal: the twentieth link of a long walk is added long after the yes.
func TestTheWaitForALinksDocumentCountsFromWhenItWasAdded(t *testing.T) {
	e := newEnv(t, factsAnswer)
	doc := e.document(t, "late.pdf", "pending")
	e.runner.WithLinks(&fakeLinks{docs: map[string]db.IngestedFile{"https://a.example/p.pdf": doc}})
	e.startLinks(t, KindKeyFacts, "s", "https://a.example/p.pdf")
	later := time.Now().Add(waitForIngest + time.Minute)
	e.runner.now = func() time.Time { return later }

	e.walk(t) // added "now", that is long after the proposal

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepPending, p.Steps[0].Status, "not timed out: it was just added")

	e.runner.now = func() time.Time { return later.Add(waitForIngest + time.Minute) }
	e.walk(t)
	p = e.plan(t, "s")
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Equal(t, "документът не се обработи навреме", p.Steps[0].Note)
}

func TestALinkTheSearchFoundIsUsedOnlyWhenItsDocumentIsTheExpectedWork(t *testing.T) {
	e := newEnv(t, factsAnswer)
	other := e.document(t, "thesis.pdf", "completed", "A thesis on momentum. George and Hwang (2004) also suggest 1.41 and 1.52, 1.63, 1.74.")
	right := e.document(t, "vol.pdf", "completed", "ON THE PERFORMANCE OF VOLATILITY-\nMANAGED PORTFOLIOS\nScott Cederburg. Returns 1.41, 1.52, 1.63 and 1.74.")
	proposed, err := e.plans.Propose(e.ctx, e.user, "s", KindKeyFacts, "ключовите факти на всеки линк", []taskplan.Step{
		{ID: 1, Text: "Maxing out", Ref: other.ID.String(), Expect: "Maxing out", Status: taskplan.StepPending},
		{ID: 2, Text: "Volatility", Ref: right.ID.String(), Expect: "On the Performance of Volatility-Managed Portfolios", Status: taskplan.StepPending},
	})
	require.NoError(t, err)
	_, err = e.plans.Start(e.ctx, e.user, proposed.ID)
	require.NoError(t, err)

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Contains(t, p.Steps[0].Note, "не е тази статия")
	assert.Equal(t, taskplan.StepDone, p.Steps[1].Status, p.Steps[1].Note)
	assert.Len(t, e.llm.requests, 1, "no notes are asked for of another work")
}
