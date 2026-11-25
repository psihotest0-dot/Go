package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/bills"
	"go-monolith/internal/reminders"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
)

// The tests of waiting_proposals.go: a "да" is for the question asked last.

// withTasks lets o walk tasks over many documents, as TASK_RUN_ENABLED does.
func withTasks(e *attachmentEnv, o *Orchestrator) *taskplan.Store {
	plans := taskplan.NewStore(e.db)
	o.WithTaskRun(plans, &countingWaker{})
	return plans
}

// proposeTask stores a task the server would walk after a yes, as taskRunAnswer does.
func proposeTask(t *testing.T, e *attachmentEnv, plans *taskplan.Store, session string) *taskplan.Plan {
	t.Helper()
	p, err := plans.Propose(context.Background(), e.userID, session, taskrun.KindKeyFacts, "файл с ключовите факти за всеки от 1 документа",
		[]taskplan.Step{{ID: 1, Text: "paper-a.pdf", Status: taskplan.StepPending, Ref: "00000000-0000-4000-8000-000000000001"}})
	require.NoError(t, err)
	return p
}

// runTask makes the server walk a task in the conversation, as after its yes.
func runTask(t *testing.T, e *attachmentEnv, plans *taskplan.Store, session string) {
	t.Helper()
	_, err := plans.Start(context.Background(), e.userID, proposeTask(t, e, plans, session).ID)
	require.NoError(t, err)
}

func keptBills(t *testing.T, e *billEnv) int64 {
	t.Helper()
	n, err := e.db.Queries.CountKeptBills(context.Background(), e.userID)
	require.NoError(t, err)
	return n
}

func TestAYesWhileATaskRunsKeepsTheBillThatWaits(t *testing.T) {
	e := newBillEnv(t)
	plans := withTasks(e.attachmentEnv, e.o)
	runTask(t, e.attachmentEnv, plans, e.session)

	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	out := e.say(t, "да")
	assert.Contains(t, out, "Записах сметката ✅", "the yes is for the bill asked about last")
	assert.NotContains(t, out, "Вече работя по нея")
	assert.Equal(t, int64(1), keptBills(t, e))

	running, err := plans.Running(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.NotNil(t, running, "the task goes on")
	assert.Contains(t, e.say(t, "да"), "Вече работя по нея.", "with nothing else waiting a yes is for the task again")
}

func TestAYesWhileATaskRunsKeepsTheReminderThatWaits(t *testing.T) {
	e := newReminderEnv(t)
	plans := withTasks(e.attachmentEnv, e.o)
	runTask(t, e.attachmentEnv, plans, e.session)
	e.reads(t, reminders.Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "09:00", TimeGiven: true, Repeat: "none"})

	assert.Contains(t, e.say(t, "напомни ми утре в 9 да платя тока"), "Така ли?")
	out := e.say(t, "да")
	assert.Contains(t, out, "Записах ✅")
	assert.NotContains(t, out, "Вече работя по нея")
	active, err := e.store.Active(context.Background(), e.userID)
	require.NoError(t, err)
	assert.Len(t, active, 1)
}

func TestABillUploadedAfterATaskWasProposedTakesTheYes(t *testing.T) {
	e := newBillEnv(t)
	plans := withTasks(e.attachmentEnv, e.o)
	proposeTask(t, e.attachmentEnv, plans, e.session)

	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	proposed, err := plans.Proposed(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, proposed, "the upload asks a new question: the task that waited is dropped")

	assert.Contains(t, e.say(t, "да"), "Записах сметката ✅")
	running, err := plans.Running(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, running, "the yes for the bill starts no task")
}

func TestATaskProposedAfterABillTakesTheYes(t *testing.T) {
	e := newBillEnv(t)
	plans := withTasks(e.attachmentEnv, e.o)
	docs := &taskRunEnv{attachmentEnv: e.attachmentEnv}
	storeTurnMessages(t, e.attachmentEnv, e.session, [2]string{"user", "добави тези в библиотеката: https://example.org/a.pdf"},
		[2]string{"assistant", "Добавих го."})
	docs.addDocument(t, "paper-a.pdf", "")

	e.reads(t, madeUpFields)
	_, err := e.svc.Read(context.Background(), e.userID, e.session, "rechnung.pdf", []byte("%PDF-1.4 one"))
	require.NoError(t, err)
	assert.Contains(t, e.say(t, askForFacts), "Да започна ли?")
	bill, err := e.svc.Proposed(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, bill, "the task is the new question: the bill that waited is dropped")

	assert.Contains(t, e.say(t, "да"), "Започнах.")
	assert.Zero(t, keptBills(t, e), "the yes for the task keeps no bill")
	running, err := plans.Running(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.NotNil(t, running)
}

func TestAReminderOfTheButtonTakesTheYesFromATaskThatWaited(t *testing.T) {
	e := newReminderEnv(t)
	plans := withTasks(e.attachmentEnv, e.o)
	proposeTask(t, e.attachmentEnv, plans, e.session)
	e.reads(t, reminders.Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "09:00", TimeGiven: true, Repeat: "none"})

	// The button skips the answer of the tasks, which drops a proposal on any other message.
	code, out := (&taskRunEnv{attachmentEnv: e.attachmentEnv, o: e.o}).sayIn(t, e.session, "утре в 9 да платя тока", ModeReminder)
	require.Equal(t, 200, code, out)
	assert.Contains(t, out, "Така ли?")
	out = e.say(t, "да")
	assert.Contains(t, out, "Записах ✅")
	assert.NotContains(t, out, "Започнах.")
	running, err := plans.Running(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, running)
	proposed, err := plans.Proposed(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, proposed)
}

// The standing bill proposed in the chat drops a task that waited too (in its transaction).
func TestAStandingBillProposedAfterATaskTakesTheYes(t *testing.T) {
	e := newBillEnv(t)
	plans := withTasks(e.attachmentEnv, e.o)
	p := proposeTask(t, e.attachmentEnv, plans, e.session)
	e.readsStanding(t, bills.StandingFields{Issuer: "Beispiel Spiele Club", Kind: "subscription", Summary: "Абонамент за игри"})

	// The proposal is made by the service itself, as a request in the chat would after a message
	// that did not go through the answer of the tasks.
	text, handled := e.svc.ProposeStanding(context.Background(), e.userID, e.session, "запише че на всяка 28 дата имам сметка от 14 евро към beispeil spiele club")
	require.True(t, handled)
	require.Contains(t, text, "Да я запиша ли в базата?")
	proposed, err := plans.Proposed(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, proposed, "the task %d that waited is dropped", p.ID)
	var count int
	require.NoError(t, e.db.SqlDB.QueryRow(`SELECT count(*) FROM task_plans WHERE user_id = $1`, e.userID).Scan(&count))
	assert.Zero(t, count)
}
