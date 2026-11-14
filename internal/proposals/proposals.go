// Package proposals holds the writes the model asks for that outlive the
// conversation: a global rule, a note on how to talk with the user, or a memory.
//
// A global rule is the first thing in the system prompt of every session, and
// text from a web page or a book can talk the model into saving one. So the
// tools do not write such things themselves; they leave a proposal that the user
// confirms or rejects (HTTP API for the web client, commands in Telegram). A
// memory is saved directly only when the turn read nothing but the conversation.
package proposals

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
	"go-monolith/metrics"
)

// The kinds of proposal; they are the names of the tools that make them.
const (
	KindSaveRule      = "save_global_rule"
	KindSaveStyleNote = "save_style_note"
	KindCreateMemory  = "create_memory"
)

// The kinds of row in global_rules: a rule is an instruction the user gave, a style
// note an observation of how he likes to be talked to (plan item P42). Both are in
// every prompt, each in its own section.
const (
	RuleKindRule  = "rule"
	RuleKindStyle = "style"
)

// Statuses of a proposal.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

const (
	// MaxRuleKeyRunes and MaxRuleValueRunes bound a rule. The value limit is the
	// per-rule cap of the system prompt (maxRuleChars in the agent): a longer rule
	// would be cut there anyway.
	MaxRuleKeyRunes   = 64
	MaxRuleValueRunes = 400

	// MaxActiveRules bounds the rules one user can have. Rules are always in the
	// prompt, so each one costs context on every turn.
	MaxActiveRules = 20

	// MaxStyleNotes bounds the notes on how to talk with the user. A handful of
	// observations shape the tone; a long list would be read as noise.
	MaxStyleNotes = 8

	// MaxMemoryRunes bounds a memory written by a tool. The prompt shows at most
	// 800 characters of a memory, and the tool is for short facts.
	MaxMemoryRunes = 1000

	// MinImportance, MaxImportance and DefaultImportance are the importance of a
	// memory: 1 = trivia, 10 = critical, as in the HTTP API.
	MinImportance     = 1
	MaxImportance     = 10
	DefaultImportance = 5

	// MaxPending bounds the proposals one user has open, so a looping model (or a
	// page that keeps asking) cannot bury the ones that matter.
	MaxPending = 20

	// PendingTTL is how long a proposal waits for an answer.
	PendingTTL = 14 * 24 * time.Hour
)

var (
	// ErrNotFound: no such proposal for this user (the same for another user's id).
	ErrNotFound = errors.New("proposal not found")
	// ErrNotPending: the proposal has been decided already.
	ErrNotPending = errors.New("proposal already decided")
	// ErrTooManyPending: the user has MaxPending open proposals.
	ErrTooManyPending = errors.New("too many proposals are waiting for an answer")
	// ErrTooManyRules: the user has MaxActiveRules rules.
	ErrTooManyRules = errors.New("too many global rules")
	// ErrTooManyStyleNotes: the user has MaxStyleNotes notes.
	ErrTooManyStyleNotes = errors.New("too many notes on how to talk")
)

// InvalidError is a proposal the model got wrong; its message says how to fix the
// call.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

// RulePayload is the stored form of a save_global_rule or save_style_note proposal.
type RulePayload struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// MemoryPayload is the stored form of a create_memory proposal.
type MemoryPayload struct {
	Content    string `json:"content"`
	Importance int    `json:"importance"`
}

// NormalizeRule trims and checks the arguments of save_global_rule.
func NormalizeRule(key, value string) (RulePayload, error) {
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if key == "" || value == "" {
		return RulePayload{}, &InvalidError{"a rule needs a non-empty key and value"}
	}
	if n := utf8.RuneCountInString(key); n > MaxRuleKeyRunes {
		return RulePayload{}, &InvalidError{fmt.Sprintf("the rule key is %d characters; the limit is %d", n, MaxRuleKeyRunes)}
	}
	if n := utf8.RuneCountInString(value); n > MaxRuleValueRunes {
		return RulePayload{}, &InvalidError{fmt.Sprintf("the rule is %d characters; the limit is %d, make it shorter", n, MaxRuleValueRunes)}
	}
	return RulePayload{Key: key, Value: value}, nil
}

// NormalizeMemory trims and checks the arguments of create_memory. An importance
// of 0 means "not given".
func NormalizeMemory(content string, importance int) (MemoryPayload, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return MemoryPayload{}, &InvalidError{"a memory needs a non-empty content"}
	}
	if n := utf8.RuneCountInString(content); n > MaxMemoryRunes {
		return MemoryPayload{}, &InvalidError{fmt.Sprintf("the memory is %d characters; the limit is %d, keep one short fact", n, MaxMemoryRunes)}
	}
	if importance == 0 {
		importance = DefaultImportance
	}
	if importance < MinImportance || importance > MaxImportance {
		return MemoryPayload{}, &InvalidError{fmt.Sprintf("importance must be between %d and %d", MinImportance, MaxImportance)}
	}
	return MemoryPayload{Content: content, Importance: importance}, nil
}

// Proposal is a proposal as the API and the bots show it.
type Proposal struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	SessionID string `json:"session_id"`
	// Summary is a one line description in Bulgarian, ready to show.
	Summary string `json:"summary"`
	// Key and Value are set for a rule, Content and Importance for a memory.
	Key        string `json:"key,omitempty"`
	Value      string `json:"value,omitempty"`
	Content    string `json:"content,omitempty"`
	Importance int    `json:"importance,omitempty"`
	// ExternalSources lists where untrusted text came from in the turn that made
	// the proposal; empty means only the conversation.
	ExternalSources []string  `json:"external_sources"`
	CreatedAt       time.Time `json:"created_at"`
}

func toProposal(row db.PendingAction) (Proposal, error) {
	p := Proposal{
		ID:              row.ID,
		Kind:            row.Kind,
		Status:          row.Status,
		SessionID:       row.SessionID,
		ExternalSources: row.ExternalSources,
		CreatedAt:       row.CreatedAt,
	}
	if p.ExternalSources == nil {
		p.ExternalSources = []string{}
	}

	switch row.Kind {
	case KindSaveRule:
		var payload RulePayload
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return Proposal{}, fmt.Errorf("proposal %d has an unreadable payload: %w", row.ID, err)
		}
		p.Key, p.Value = payload.Key, payload.Value
		p.Summary = fmt.Sprintf("Правило „%s“: %s", payload.Key, payload.Value)
	case KindSaveStyleNote:
		var payload RulePayload
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return Proposal{}, fmt.Errorf("proposal %d has an unreadable payload: %w", row.ID, err)
		}
		p.Key, p.Value = payload.Key, payload.Value
		p.Summary = "Как говорим: " + payload.Value
	case KindCreateMemory:
		var payload MemoryPayload
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return Proposal{}, fmt.Errorf("proposal %d has an unreadable payload: %w", row.ID, err)
		}
		p.Content, p.Importance = payload.Content, payload.Importance
		p.Summary = "Спомен: " + payload.Content
	default:
		return Proposal{}, fmt.Errorf("proposal %d has an unknown kind %q", row.ID, row.Kind)
	}
	return p, nil
}

// Service stores and decides proposals.
type Service struct {
	db        *db.Database
	retrieval *retrieval.RetrievalService
	now       func() time.Time
}

// NewService creates a Service.
func NewService(database *db.Database, r *retrieval.RetrievalService) *Service {
	return &Service{db: database, retrieval: r, now: time.Now}
}

// ProposeRule leaves a save_global_rule proposal. external names the sources of
// untrusted text the turn read.
func (s *Service) ProposeRule(ctx context.Context, userID uuid.UUID, sessionID, key, value string, external []string) (Proposal, error) {
	payload, err := NormalizeRule(key, value)
	if err != nil {
		return Proposal{}, err
	}
	// Fail now, while the model can still tell the user, instead of at approval.
	if err := s.checkRuleRoom(ctx, userID, RuleKindRule, payload.Key); err != nil {
		return Proposal{}, err
	}
	return s.propose(ctx, userID, sessionID, KindSaveRule, payload, external)
}

// ProposeStyleNote leaves a save_style_note proposal: an observation of how the user
// likes to be talked to. Like a rule it is in every prompt, so it waits for the
// user's confirmation; a wrong generalisation would bend every conversation.
func (s *Service) ProposeStyleNote(ctx context.Context, userID uuid.UUID, sessionID, key, value string, external []string) (Proposal, error) {
	payload, err := NormalizeRule(key, value)
	if err != nil {
		return Proposal{}, err
	}
	if err := s.checkRuleRoom(ctx, userID, RuleKindStyle, payload.Key); err != nil {
		return Proposal{}, err
	}
	return s.propose(ctx, userID, sessionID, KindSaveStyleNote, payload, external)
}

// ProposeMemory leaves a create_memory proposal.
func (s *Service) ProposeMemory(ctx context.Context, userID uuid.UUID, sessionID, content string, importance int, external []string) (Proposal, error) {
	payload, err := NormalizeMemory(content, importance)
	if err != nil {
		return Proposal{}, err
	}
	return s.propose(ctx, userID, sessionID, KindCreateMemory, payload, external)
}

func (s *Service) propose(ctx context.Context, userID uuid.UUID, sessionID, kind string, payload any, external []string) (Proposal, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Proposal{}, fmt.Errorf("failed to encode the proposal: %w", err)
	}

	// Old unanswered proposals expire, so they do not fill the limit forever.
	if _, err := s.db.Queries.DeleteStalePendingActions(ctx, db.DeleteStalePendingActionsParams{
		UserID:    userID,
		CreatedAt: s.now().Add(-PendingTTL),
	}); err != nil {
		return Proposal{}, fmt.Errorf("failed to expire old proposals: %w", err)
	}

	open, err := s.db.Queries.CountPendingActions(ctx, userID)
	if err != nil {
		return Proposal{}, fmt.Errorf("failed to count open proposals: %w", err)
	}
	if open >= MaxPending {
		return Proposal{}, ErrTooManyPending
	}

	if external == nil {
		external = []string{}
	}
	sum := sha256.Sum256(append([]byte(kind+"\x00"), raw...))
	row, err := s.db.Queries.CreatePendingAction(ctx, db.CreatePendingActionParams{
		UserID:          userID,
		SessionID:       sessionID,
		Kind:            kind,
		Payload:         raw,
		PayloadHash:     hex.EncodeToString(sum[:]),
		ExternalSources: external,
	})
	if err != nil {
		return Proposal{}, fmt.Errorf("failed to store the proposal: %w", err)
	}

	slog.Info("proposal created",
		"id", row.ID, "kind", kind, "user_id", userID, "session_id", sessionID, "external_sources", external)
	metrics.Proposals.WithLabelValues(kind, "created").Inc()
	return toProposal(row)
}

// Pending lists the open proposals of a user, oldest first.
func (s *Service) Pending(ctx context.Context, userID uuid.UUID) ([]Proposal, error) {
	rows, err := s.db.Queries.ListPendingActions(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list proposals: %w", err)
	}
	out := make([]Proposal, 0, len(rows))
	cutoff := s.now().Add(-PendingTTL)
	for _, row := range rows {
		if row.CreatedAt.Before(cutoff) {
			continue
		}
		p, err := toProposal(row)
		if err != nil {
			slog.Error("skipping an unreadable proposal", "err", err)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// Reject closes a proposal without applying it.
func (s *Service) Reject(ctx context.Context, userID uuid.UUID, id int64) (Proposal, error) {
	row, err := s.claim(ctx, userID, id, StatusRejected)
	if err != nil {
		return Proposal{}, err
	}
	slog.Info("proposal rejected", "id", id, "kind", row.Kind, "user_id", userID)
	metrics.Proposals.WithLabelValues(row.Kind, "rejected").Inc()
	return toProposal(row)
}

// Approve applies a proposal: the rule or the note is saved, the memory is created. The
// proposal is claimed first, so two taps cannot apply it twice; if applying
// fails it is reopened and the user can try again.
func (s *Service) Approve(ctx context.Context, userID uuid.UUID, id int64) (Proposal, error) {
	row, err := s.claim(ctx, userID, id, StatusApproved)
	if err != nil {
		return Proposal{}, err
	}

	if err := s.apply(ctx, userID, row); err != nil {
		// The caller's context may be what failed, so the reopening must not use it.
		reopenCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if reopenErr := s.db.Queries.ReopenPendingAction(reopenCtx, db.ReopenPendingActionParams{
			ID: id, UserID: userID, Status: StatusApproved,
		}); reopenErr != nil {
			slog.Error("failed to reopen a proposal that could not be applied", "id", id, "err", reopenErr)
		}
		return Proposal{}, err
	}

	slog.Info("proposal approved", "id", id, "kind", row.Kind, "user_id", userID)
	metrics.Proposals.WithLabelValues(row.Kind, "approved").Inc()
	return toProposal(row)
}

// claim moves an open proposal to status, or says why it cannot.
func (s *Service) claim(ctx context.Context, userID uuid.UUID, id int64, status string) (db.PendingAction, error) {
	row, err := s.db.Queries.DecidePendingAction(ctx, db.DecidePendingActionParams{ID: id, UserID: userID, Status: status})
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.PendingAction{}, fmt.Errorf("failed to decide the proposal: %w", err)
	}

	// Either it never existed for this user or someone decided it first.
	if _, getErr := s.db.Queries.GetPendingAction(ctx, db.GetPendingActionParams{ID: id, UserID: userID}); getErr != nil {
		if errors.Is(getErr, sql.ErrNoRows) {
			return db.PendingAction{}, ErrNotFound
		}
		return db.PendingAction{}, fmt.Errorf("failed to read the proposal: %w", getErr)
	}
	return db.PendingAction{}, ErrNotPending
}

func (s *Service) apply(ctx context.Context, userID uuid.UUID, row db.PendingAction) error {
	switch row.Kind {
	case KindSaveRule, KindSaveStyleNote:
		var payload RulePayload
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return fmt.Errorf("proposal %d has an unreadable payload: %w", row.ID, err)
		}
		kind := RuleKindRule
		if row.Kind == KindSaveStyleNote {
			kind = RuleKindStyle
		}
		if err := s.checkRuleRoom(ctx, userID, kind, payload.Key); err != nil {
			return err
		}
		if err := s.db.Queries.SaveGlobalRule(ctx, db.SaveGlobalRuleParams{
			UserID: userID, Kind: kind, Key: payload.Key, Value: payload.Value,
		}); err != nil {
			return fmt.Errorf("failed to save the %s: %w", kind, err)
		}
		return nil

	case KindCreateMemory:
		var payload MemoryPayload
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return fmt.Errorf("proposal %d has an unreadable payload: %w", row.ID, err)
		}
		metadata, _ := json.Marshal(map[string]any{
			"source":           "approved_proposal",
			"proposal_id":      row.ID,
			"external_sources": row.ExternalSources,
		})
		_, err := s.retrieval.CreateMemory(ctx, userID, payload.Content, payload.Importance, "active",
			pqtype.NullRawMessage{RawMessage: metadata, Valid: true})
		if err != nil && !errors.Is(err, db.ErrMemoryExists) {
			return fmt.Errorf("failed to save the memory: %w", err)
		}
		return nil
	}
	return fmt.Errorf("proposal %d has an unknown kind %q", row.ID, row.Kind)
}

// checkRuleRoom fails when saving key as a row of kind (RuleKindRule or
// RuleKindStyle) would exceed the limit of that kind (MaxActiveRules, MaxStyleNotes).
// Replacing an existing one always has room.
func (s *Service) checkRuleRoom(ctx context.Context, userID uuid.UUID, kind, key string) error {
	exists, err := s.db.Queries.GlobalRuleExists(ctx, db.GlobalRuleExistsParams{UserID: userID, Kind: kind, Key: key})
	if err != nil {
		return fmt.Errorf("failed to look up the %s: %w", kind, err)
	}
	if exists {
		return nil
	}
	count, err := s.db.Queries.CountGlobalRules(ctx, db.CountGlobalRulesParams{UserID: userID, Kind: kind})
	if err != nil {
		return fmt.Errorf("failed to count the %ss: %w", kind, err)
	}
	if kind == RuleKindStyle {
		if count >= MaxStyleNotes {
			return ErrTooManyStyleNotes
		}
		return nil
	}
	if count >= MaxActiveRules {
		return ErrTooManyRules
	}
	return nil
}
