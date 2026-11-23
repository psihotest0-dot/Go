package taskplan

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"go-monolith/internal/db"
)

// keepPlans is how many plans of a conversation are kept (the active one and the newest of the
// others); a plan that is over is only there to read what was done.
const keepPlans = 5

// Store keeps the plans in PostgreSQL. One plan of a conversation is active at a time; the
// callers hold the lock of the conversation (agent.sessionLocks), so a read and a write of a
// plan do not interleave.
type Store struct {
	d *db.Database
	q *db.Queries
}

// NewStore returns the plans stored in d.
func NewStore(d *db.Database) *Store { return &Store{d: d, q: d.Queries} }

func toPlan(row db.TaskPlan) (*Plan, error) {
	p := &Plan{ID: row.ID, Goal: row.Goal, Status: row.Status, Kind: row.Kind, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if err := json.Unmarshal(row.Steps, &p.Steps); err != nil {
		return nil, fmt.Errorf("plan %d has unreadable steps: %w", row.ID, err)
	}
	if len(row.Ledger) > 0 {
		if err := json.Unmarshal(row.Ledger, &p.Ledger); err != nil {
			return nil, fmt.Errorf("plan %d has an unreadable ledger: %w", row.ID, err)
		}
	}
	return p, nil
}

// Active returns the plan of the conversation that is in progress, or nil when there is none.
func (s *Store) Active(ctx context.Context, userID uuid.UUID, session string) (*Plan, error) {
	return planOrNil(s.q.GetActiveTaskPlan(ctx, db.GetActiveTaskPlanParams{UserID: userID, SessionID: session}))
}

// Latest returns the newest plan of the conversation, whatever became of it, or nil.
func (s *Store) Latest(ctx context.Context, userID uuid.UUID, session string) (*Plan, error) {
	return planOrNil(s.q.GetLatestTaskPlan(ctx, db.GetLatestTaskPlanParams{UserID: userID, SessionID: session}))
}

// planOrNil is the plan of a row that was read, or nil when there was no row.
func planOrNil(row db.TaskPlan, err error) (*Plan, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	return toPlan(row)
}

// Set makes a new plan for the conversation; the plan in progress, if any, is cancelled. The
// error is an *InvalidError when the model gave a bad goal or steps. The cancel, the new plan
// and the trim are one transaction: a failure leaves the plan in progress as it was, and the
// model is never told that a plan failed which is in fact there.
func (s *Store) Set(ctx context.Context, userID uuid.UUID, session, goal string, inputs []StepInput) (*Plan, error) {
	p, err := New(goal, inputs)
	if err != nil {
		return nil, err
	}
	steps, err := json.Marshal(p.Steps)
	if err != nil {
		return nil, fmt.Errorf("encode the steps: %w", err)
	}
	tx, err := s.d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	q := s.q.WithTx(tx)
	if err := q.CancelActiveTaskPlan(ctx, db.CancelActiveTaskPlanParams{UserID: userID, SessionID: session}); err != nil {
		return nil, fmt.Errorf("cancel the plan in progress: %w", err)
	}
	row, err := q.CreateTaskPlan(ctx, db.CreateTaskPlanParams{
		UserID: userID, SessionID: session, Goal: p.Goal, Steps: steps, Ledger: json.RawMessage("[]"),
	})
	if err != nil {
		return nil, fmt.Errorf("make the plan: %w", err)
	}
	// The old ones are only for reading; they do not pile up.
	if err := q.TrimTaskPlans(ctx, db.TrimTaskPlansParams{UserID: userID, SessionID: session, KeepCount: keepPlans}); err != nil {
		return nil, fmt.Errorf("trim the plans: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit the plan: %w", err)
	}
	made, err := toPlan(row)
	if err != nil {
		return nil, err
	}
	made.Warnings = p.Warnings
	return made, nil
}

// save writes the steps, the ledger and the status of p.
func (s *Store) save(ctx context.Context, userID uuid.UUID, p *Plan) error {
	steps, err := json.Marshal(p.Steps)
	if err != nil {
		return fmt.Errorf("encode the steps: %w", err)
	}
	ledger, err := json.Marshal(p.Ledger)
	if err != nil {
		return fmt.Errorf("encode the ledger: %w", err)
	}
	if p.Ledger == nil {
		ledger = []byte("[]")
	}
	return s.q.UpdateTaskPlan(ctx, db.UpdateTaskPlanParams{ID: p.ID, UserID: userID, Steps: steps, Ledger: ledger, Status: p.Status})
}

// Record writes what a tool did in the ledger of the plan in progress; nothing happens when the
// conversation has none. The plan is returned (nil when there is none).
func (s *Store) Record(ctx context.Context, userID uuid.UUID, session, tool, gist string) (*Plan, error) {
	p, err := s.Active(ctx, userID, session)
	if err != nil || p == nil {
		return nil, err
	}
	p.AddEvidence(tool, gist)
	if err := s.save(ctx, userID, p); err != nil {
		return nil, fmt.Errorf("record in the plan: %w", err)
	}
	return p, nil
}

// Update sets the status of a step of the plan in progress. The error is an *InvalidError when
// the call is wrong (no plan, no such step, a step that cannot be done yet).
func (s *Store) Update(ctx context.Context, userID uuid.UUID, session string, stepID int, status, note string) (Outcome, *Plan, error) {
	p, err := s.Active(ctx, userID, session)
	if err != nil {
		return Outcome{}, nil, err
	}
	if p == nil {
		return Outcome{}, nil, &InvalidError{"there is no plan in progress; make one with plan_set"}
	}
	out, err := p.Update(stepID, status, note)
	if err != nil {
		return Outcome{}, p, err
	}
	if err := s.save(ctx, userID, p); err != nil {
		return Outcome{}, nil, fmt.Errorf("save the plan: %w", err)
	}
	return out, p, nil
}

// Cancel ends the plan in progress, if any.
func (s *Store) Cancel(ctx context.Context, userID uuid.UUID, session string) error {
	return s.q.CancelActiveTaskPlan(ctx, db.CancelActiveTaskPlanParams{UserID: userID, SessionID: session})
}

// The plans the server walks itself (Kind is set; package taskrun). They are made as proposals,
// started by the person's yes, and their steps are written by the server only.

// ErrNotStarted: the proposal was not waiting any more, or a plan of its kind is being walked.
var ErrNotStarted = errors.New("the plan was not started")

// Propose stores a plan the server will walk once the person says yes; it takes the place of a
// proposal that waits in the conversation. The steps are made by the code, so nothing is mended.
func (s *Store) Propose(ctx context.Context, userID uuid.UUID, session, kind, goal string, steps []Step) (*Plan, error) {
	if kind == "" || goal == "" || len(steps) == 0 || len(steps) > MaxSteps {
		return nil, fmt.Errorf("a proposal needs a kind, a goal and 1 to %d steps (%q, %d)", MaxSteps, kind, len(steps))
	}
	raw, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("encode the steps: %w", err)
	}
	tx, err := s.d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	q := s.q.WithTx(tx)
	if err := q.DropProposedTaskPlan(ctx, db.DropProposedTaskPlanParams{UserID: userID, SessionID: session}); err != nil {
		return nil, fmt.Errorf("drop the waiting proposal: %w", err)
	}
	row, err := q.CreateProposedTaskPlan(ctx, db.CreateProposedTaskPlanParams{
		UserID: userID, SessionID: session, Goal: cut(goal, MaxGoalRunes), Steps: raw, Kind: kind,
	})
	if err != nil {
		return nil, fmt.Errorf("store the proposal: %w", err)
	}
	if err := q.TrimTaskPlans(ctx, db.TrimTaskPlansParams{UserID: userID, SessionID: session, KeepCount: keepPlans}); err != nil {
		return nil, fmt.Errorf("trim the plans: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit the proposal: %w", err)
	}
	return toPlan(row)
}

// Proposed is the proposal that waits for the person's yes in the conversation, or nil.
func (s *Store) Proposed(ctx context.Context, userID uuid.UUID, session string) (*Plan, error) {
	return planOrNil(s.q.GetProposedTaskPlan(ctx, db.GetProposedTaskPlanParams{UserID: userID, SessionID: session}))
}

// DropProposed forgets the proposal of the conversation, if any (the person asked for something else).
func (s *Store) DropProposed(ctx context.Context, userID uuid.UUID, session string) error {
	return s.q.DropProposedTaskPlan(ctx, db.DropProposedTaskPlanParams{UserID: userID, SessionID: session})
}

// Start makes the proposal the plan the server walks. ErrNotStarted when it was not waiting any
// more, or a plan of its kind is being walked in the conversation.
func (s *Store) Start(ctx context.Context, userID uuid.UUID, id int64) (*Plan, error) {
	row, err := s.q.StartProposedTaskPlan(ctx, db.StartProposedTaskPlanParams{ID: id, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotStarted
	}
	if err != nil {
		return nil, fmt.Errorf("start the plan: %w", err)
	}
	return toPlan(row)
}

// Running is the plan the server walks in the conversation, or nil.
func (s *Store) Running(ctx context.Context, userID uuid.UUID, session string) (*Plan, error) {
	return planOrNil(s.q.GetRunningTaskPlan(ctx, db.GetRunningTaskPlanParams{UserID: userID, SessionID: session}))
}

// RunPlan is a plan the server walks, with the conversation it belongs to.
type RunPlan struct {
	*Plan
	UserID  uuid.UUID
	Session string
}

// AllRunning are the plans the server walks, of every conversation, the oldest first.
func (s *Store) AllRunning(ctx context.Context) ([]RunPlan, error) {
	rows, err := s.q.ListRunningTaskPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the running plans: %w", err)
	}
	out := make([]RunPlan, 0, len(rows))
	for _, row := range rows {
		p, err := toPlan(row)
		if err != nil {
			return nil, err
		}
		out = append(out, RunPlan{Plan: p, UserID: row.UserID, Session: row.SessionID})
	}
	return out, nil
}

// SaveRun writes the steps and the status of a plan the server walks. false: the plan is not
// active any more (the person stopped it), and nothing was written.
func (s *Store) SaveRun(ctx context.Context, userID uuid.UUID, p *Plan) (bool, error) {
	raw, err := json.Marshal(p.Steps)
	if err != nil {
		return false, fmt.Errorf("encode the steps: %w", err)
	}
	n, err := s.q.SaveRunningTaskPlan(ctx, db.SaveRunningTaskPlanParams{ID: p.ID, UserID: userID, Steps: raw, Status: p.Status})
	if err != nil {
		return false, fmt.Errorf("save the plan: %w", err)
	}
	return n > 0, nil
}

// StopRun stops the plan the server walks in the conversation; false when none was walked.
func (s *Store) StopRun(ctx context.Context, userID uuid.UUID, session string) (bool, error) {
	n, err := s.q.CancelRunningTaskPlans(ctx, db.CancelRunningTaskPlansParams{UserID: userID, SessionID: session})
	if err != nil {
		return false, fmt.Errorf("stop the plan: %w", err)
	}
	return n > 0, nil
}

// LatestRun is the newest plan the server walked or walks in the conversation, or nil.
func (s *Store) LatestRun(ctx context.Context, userID uuid.UUID, session string) (*Plan, error) {
	return planOrNil(s.q.GetLatestRunTaskPlan(ctx, db.GetLatestRunTaskPlanParams{UserID: userID, SessionID: session}))
}

// LatestAny is the newest plan of the conversation that is not a proposal, the model's or the
// server's, or nil: what the app shows.
func (s *Store) LatestAny(ctx context.Context, userID uuid.UUID, session string) (*Plan, error) {
	model, err := s.Latest(ctx, userID, session)
	if err != nil {
		return nil, err
	}
	run, err := s.LatestRun(ctx, userID, session)
	if err != nil {
		return nil, err
	}
	if run != nil && (model == nil || run.ID > model.ID) {
		return run, nil
	}
	return model, nil
}
