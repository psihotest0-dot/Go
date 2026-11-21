package taskplan

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

type env struct {
	store *Store
	db    *db.Database
	user  uuid.UUID
	ctx   context.Context
}

func newEnv(t *testing.T) *env {
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
	return &env{store: NewStore(database), db: database, user: user.ID, ctx: ctx}
}

func (e *env) count(t *testing.T, session, status string) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.SqlDB.QueryRowContext(e.ctx,
		`SELECT count(*) FROM task_plans WHERE user_id = $1 AND session_id = $2 AND ($3 = '' OR status = $3)`, e.user, session, status).Scan(&n))
	return n
}

func TestNoPlanIsNil(t *testing.T) {
	e := newEnv(t)

	active, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Nil(t, active)
	latest, err := e.store.Latest(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Nil(t, latest)
	recorded, err := e.store.Record(e.ctx, e.user, "s", "web_fetch", "x")
	require.NoError(t, err, "a tool call in a conversation without a plan is nothing")
	assert.Nil(t, recorded)
}

func TestAPlanIsKeptAndRead(t *testing.T) {
	e := newEnv(t)

	made, err := e.store.Set(e.ctx, e.user, "s", "свали източниците", []StepInput{
		{Text: "намери линковете"}, {Text: "добави ги", Tool: "add_to_library"},
	})
	require.NoError(t, err)
	assert.NotZero(t, made.ID)
	assert.False(t, made.CreatedAt.IsZero())

	got, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, made.ID, got.ID)
	assert.Equal(t, "свали източниците", got.Goal)
	assert.Equal(t, PlanActive, got.Status)
	require.Len(t, got.Steps, 2)
	assert.Equal(t, "add_to_library", got.Steps[1].Tool)
	assert.Empty(t, got.Ledger)
}

func TestANewPlanCancelsTheOneInProgress(t *testing.T) {
	e := newEnv(t)
	first, err := e.store.Set(e.ctx, e.user, "s", "първа цел", []StepInput{{Text: "а"}})
	require.NoError(t, err)

	second, err := e.store.Set(e.ctx, e.user, "s", "втора цел", []StepInput{{Text: "б"}})
	require.NoError(t, err)

	assert.NotEqual(t, first.ID, second.ID)
	active, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Equal(t, second.ID, active.ID)
	assert.Equal(t, 1, e.count(t, "s", PlanActive))
	assert.Equal(t, 1, e.count(t, "s", PlanCancelled))
}

func TestABadPlanLeavesTheOneInProgressAlone(t *testing.T) {
	e := newEnv(t)
	first, err := e.store.Set(e.ctx, e.user, "s", "цел", []StepInput{{Text: "а"}})
	require.NoError(t, err)

	_, err = e.store.Set(e.ctx, e.user, "s", "нова цел", []StepInput{{Text: " "}})

	var invalid *InvalidError
	require.True(t, errors.As(err, &invalid))
	active, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Equal(t, first.ID, active.ID, "the plan in progress is not cancelled by a plan that was refused")
}

func TestOnlyTheNewestPlansOfAConversationStay(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 9; i++ {
		_, err := e.store.Set(e.ctx, e.user, "s", "цел", []StepInput{{Text: "а"}})
		require.NoError(t, err)
	}
	_, err := e.store.Set(e.ctx, e.user, "other", "цел", []StepInput{{Text: "а"}})
	require.NoError(t, err)

	assert.Equal(t, 5, e.count(t, "s", ""), "the newest five")
	assert.Equal(t, 1, e.count(t, "s", PlanActive), "the newest is the one in progress")
	assert.Equal(t, 1, e.count(t, "other", ""), "another conversation is not trimmed")
}

func TestWhatToolsDidIsKeptAndAStepIsDoneOnIt(t *testing.T) {
	e := newEnv(t)
	_, err := e.store.Set(e.ctx, e.user, "s", "цел", []StepInput{{Text: "свали", Tool: "add_to_library"}, {Text: "обобщи", Tool: ToolNone}})
	require.NoError(t, err)

	_, _, err = e.store.Update(e.ctx, e.user, "s", 1, StepDone, "")
	var invalid *InvalidError
	require.True(t, errors.As(err, &invalid), "no call on record yet")
	assert.Contains(t, invalid.Reason, "NOT done")

	recorded, err := e.store.Record(e.ctx, e.user, "s", "add_to_library", "add_to_library: 13 of 21 queued")
	require.NoError(t, err)
	require.NotNil(t, recorded)
	again, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	require.Len(t, again.Ledger, 1, "the ledger is in the database")
	assert.False(t, again.Ledger[0].Used)

	out, plan, err := e.store.Update(e.ctx, e.user, "s", 1, StepDone, "")
	require.NoError(t, err)
	assert.Contains(t, out.Message, "Next is step 2")
	assert.Equal(t, StepDone, plan.Steps[0].Status)
	stored, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Equal(t, StepDone, stored.Steps[0].Status)
	assert.Equal(t, "add_to_library: 13 of 21 queued", stored.Steps[0].Evidence)
	assert.True(t, stored.Ledger[0].Used)

	out, _, err = e.store.Update(e.ctx, e.user, "s", 2, StepDone, "")
	require.NoError(t, err)
	assert.True(t, out.Finished)
	finished, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Nil(t, finished, "a finished plan is not in progress")
	latest, err := e.store.Latest(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Equal(t, PlanDone, latest.Status)
}

func TestAnUpdateWithoutAPlanIsRefused(t *testing.T) {
	e := newEnv(t)

	_, _, err := e.store.Update(e.ctx, e.user, "s", 1, StepDone, "")

	var invalid *InvalidError
	require.True(t, errors.As(err, &invalid))
	assert.Contains(t, invalid.Reason, "no plan in progress")
}

func TestPlansOfConversationsAndOfPeopleAreSeparate(t *testing.T) {
	e := newEnv(t)
	stranger2, err := e.db.Queries.CreateUser(e.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.db.Pool, stranger2.ID) })
	other := &env{user: stranger2.ID}
	_, err = e.store.Set(e.ctx, e.user, "s", "моя цел", []StepInput{{Text: "а", Tool: "web_fetch"}})
	require.NoError(t, err)

	elsewhere, err := e.store.Active(e.ctx, e.user, "another-conversation")
	require.NoError(t, err)
	assert.Nil(t, elsewhere)
	stranger, err := e.store.Active(e.ctx, other.user, "s")
	require.NoError(t, err)
	assert.Nil(t, stranger, "the plan of one person is not the plan of another")
	recorded, err := e.store.Record(e.ctx, other.user, "s", "web_fetch", "x")
	require.NoError(t, err)
	assert.Nil(t, recorded)
	mine, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Empty(t, mine.Ledger, "what a stranger's tools did is not in my ledger")
}

func TestDeletingAConversationDeletesItsPlans(t *testing.T) {
	e := newEnv(t)
	_, err := e.store.Set(e.ctx, e.user, "gone", "цел", []StepInput{{Text: "а"}})
	require.NoError(t, err)
	_, err = e.store.Set(e.ctx, e.user, "stays", "цел", []StepInput{{Text: "а"}})
	require.NoError(t, err)

	res, err := e.db.DeleteSession(e.ctx, e.user, "gone")

	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Plans)
	assert.Equal(t, 0, e.count(t, "gone", ""))
	assert.Equal(t, 1, e.count(t, "stays", ""))
}

func TestCancelEndsThePlan(t *testing.T) {
	e := newEnv(t)
	_, err := e.store.Set(e.ctx, e.user, "s", "цел", []StepInput{{Text: "а"}})
	require.NoError(t, err)

	require.NoError(t, e.store.Cancel(e.ctx, e.user, "s"))

	active, err := e.store.Active(e.ctx, e.user, "s")
	require.NoError(t, err)
	assert.Nil(t, active)
	require.NoError(t, e.store.Cancel(e.ctx, e.user, "s"), "cancelling nothing is nothing")
}

func TestTheSchemaAllowsOneActivePlanPerConversation(t *testing.T) {
	e := newEnv(t)
	insert := func(session, status string) error {
		_, err := e.db.SqlDB.ExecContext(e.ctx,
			`INSERT INTO task_plans (user_id, session_id, goal, steps, status) VALUES ($1, $2, 'цел', '[]', $3)`, e.user, session, status)
		return err
	}

	require.NoError(t, insert("s", "active"))
	assert.Error(t, insert("s", "active"), "a second plan in progress")
	require.NoError(t, insert("s", "done"), "finished plans are not limited")
	require.NoError(t, insert("s", "done"))
	require.NoError(t, insert("t", "active"), "another conversation")
	assert.Error(t, insert("u", "paused"), "an unknown status")
	assert.Error(t, insert("", "active"), "no conversation")
	_, err := e.db.SqlDB.ExecContext(e.ctx, `INSERT INTO task_plans (user_id, session_id, goal, steps) VALUES ($1, 'v', 'цел', '{}')`, e.user)
	assert.Error(t, err, "steps are a list")
}
