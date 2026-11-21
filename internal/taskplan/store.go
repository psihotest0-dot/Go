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
	q *db.Queries
}

// NewStore returns the plans stored through q.
func NewStore(q *db.Queries) *Store { return &Store{q: q} }

func toPlan(row db.TaskPlan) (*Plan, error) {
	p := &Plan{ID: row.ID, Goal: row.Goal, Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
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
	row, err := s.q.GetActiveTaskPlan(ctx, db.GetActiveTaskPlanParams{UserID: userID, SessionID: session})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	return toPlan(row)
}

// Latest returns the newest plan of the conversation, whatever became of it, or nil.
func (s *Store) Latest(ctx context.Context, userID uuid.UUID, session string) (*Plan, error) {
	row, err := s.q.GetLatestTaskPlan(ctx, db.GetLatestTaskPlanParams{UserID: userID, SessionID: session})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	return toPlan(row)
}

// Set makes a new plan for the conversation; the plan in progress, if any, is cancelled. The
// error is an *InvalidError when the model gave a bad goal or steps.
func (s *Store) Set(ctx context.Context, userID uuid.UUID, session, goal string, inputs []StepInput) (*Plan, error) {
	p, err := New(goal, inputs)
	if err != nil {
		return nil, err
	}
	steps, err := json.Marshal(p.Steps)
	if err != nil {
		return nil, fmt.Errorf("encode the steps: %w", err)
	}
	if err := s.q.CancelActiveTaskPlan(ctx, db.CancelActiveTaskPlanParams{UserID: userID, SessionID: session}); err != nil {
		return nil, fmt.Errorf("cancel the plan in progress: %w", err)
	}
	row, err := s.q.CreateTaskPlan(ctx, db.CreateTaskPlanParams{
		UserID: userID, SessionID: session, Goal: p.Goal, Steps: steps, Ledger: json.RawMessage("[]"),
	})
	if err != nil {
		return nil, fmt.Errorf("make the plan: %w", err)
	}
	// The old ones are only for reading; they do not pile up.
	if err := s.q.TrimTaskPlans(ctx, db.TrimTaskPlansParams{UserID: userID, SessionID: session, KeepCount: keepPlans}); err != nil {
		return nil, fmt.Errorf("trim the plans: %w", err)
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
