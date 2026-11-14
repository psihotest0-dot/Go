package proposals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

type env struct {
	svc      *Service
	database *db.Database
	userID   uuid.UUID
	ctx      context.Context
}

// newEnv is a disposable user with a real database behind the service.
func newEnv(t *testing.T, embedder llm.Embedder) *env {
	t.Helper()

	connString := dbtest.ConnString(t)
	// db.RunMigrations resolves "migrations" relative to the working directory.
	t.Chdir("../..")

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(connString))
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	e := &env{database: database, ctx: ctx}
	e.userID = e.newUser(t)

	if embedder == nil {
		embedder = &llm.MockEmbedder{}
	}
	e.svc = NewService(database, retrieval.NewRetrievalService(database, embedder))
	return e
}

func (e *env) newUser(t *testing.T) uuid.UUID {
	t.Helper()
	user, err := e.database.Queries.CreateUser(e.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.database.Pool, user.ID) })
	return user.ID
}

func (e *env) rules(t *testing.T, userID uuid.UUID) []string {
	t.Helper()
	rules, err := e.database.Queries.GetGlobalRules(e.ctx, userID)
	require.NoError(t, err)
	return rules
}

func (e *env) memories(t *testing.T, userID uuid.UUID) []db.Memory {
	t.Helper()
	mems, err := e.database.Queries.GetMemories(e.ctx, db.GetMemoriesParams{UserID: userID})
	require.NoError(t, err)
	return mems
}

func TestNormalizeRuleLimits(t *testing.T) {
	got, err := NormalizeRule("  language ", "  Говори на български  ")
	require.NoError(t, err)
	assert.Equal(t, RulePayload{Key: "language", Value: "Говори на български"}, got)

	for name, args := range map[string][2]string{
		"empty key":      {"", "value"},
		"blank value":    {"key", "   "},
		"long key":       {strings.Repeat("к", MaxRuleKeyRunes+1), "value"},
		"long value":     {"key", strings.Repeat("я", MaxRuleValueRunes+1)},
		"long ascii key": {strings.Repeat("k", MaxRuleKeyRunes+1), "value"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeRule(args[0], args[1])
			var invalid *InvalidError
			assert.ErrorAs(t, err, &invalid)
		})
	}

	// The limits are in characters, not bytes: Cyrillic must not be penalised.
	_, err = NormalizeRule(strings.Repeat("к", MaxRuleKeyRunes), strings.Repeat("я", MaxRuleValueRunes))
	assert.NoError(t, err)
}

func TestNormalizeMemoryLimits(t *testing.T) {
	got, err := NormalizeMemory(" Обича пица ", 0)
	require.NoError(t, err)
	assert.Equal(t, MemoryPayload{Content: "Обича пица", Importance: DefaultImportance}, got, "absent importance is the default")

	for _, bad := range []int{-1, 11, 100} {
		_, err := NormalizeMemory("факт", bad)
		var invalid *InvalidError
		assert.ErrorAs(t, err, &invalid, "importance %d", bad)
	}
	for _, good := range []int{1, 5, 10} {
		_, err := NormalizeMemory("факт", good)
		assert.NoError(t, err, "importance %d", good)
	}

	_, err = NormalizeMemory("   ", 5)
	assert.Error(t, err)
	_, err = NormalizeMemory(strings.Repeat("я", MaxMemoryRunes+1), 5)
	assert.Error(t, err)
	_, err = NormalizeMemory(strings.Repeat("я", MaxMemoryRunes), 5)
	assert.NoError(t, err)
}

func TestProposedRuleIsNotSavedUntilApproved(t *testing.T) {
	e := newEnv(t, nil)

	p, err := e.svc.ProposeRule(e.ctx, e.userID, "sess", "language", "Говори на български", []string{"web_fetch"})
	require.NoError(t, err)

	assert.Equal(t, KindSaveRule, p.Kind)
	assert.Equal(t, StatusPending, p.Status)
	assert.Equal(t, "sess", p.SessionID)
	assert.Equal(t, []string{"web_fetch"}, p.ExternalSources, "the origin is kept for the user to see")
	assert.Equal(t, "Правило „language“: Говори на български", p.Summary)
	assert.Empty(t, e.rules(t, e.userID), "a proposal must not change the prompt")

	approved, err := e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, approved.Status)
	assert.Equal(t, []string{"Говори на български"}, e.rules(t, e.userID))

	pending, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestRejectedProposalChangesNothing(t *testing.T) {
	e := newEnv(t, nil)
	p, err := e.svc.ProposeRule(e.ctx, e.userID, "sess", "tone", "Бъди груба", nil)
	require.NoError(t, err)

	rejected, err := e.svc.Reject(e.ctx, e.userID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRejected, rejected.Status)
	assert.Empty(t, e.rules(t, e.userID))

	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	assert.ErrorIs(t, err, ErrNotPending, "a rejected proposal cannot be approved later")
	assert.Empty(t, e.rules(t, e.userID))
}

func TestApprovedMemoryIsCreatedWithItsOrigin(t *testing.T) {
	e := newEnv(t, nil)
	p, err := e.svc.ProposeMemory(e.ctx, e.userID, "sess", "Живее в София", 7, []string{"web_search"})
	require.NoError(t, err)
	assert.Empty(t, e.memories(t, e.userID))

	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)

	mems := e.memories(t, e.userID)
	require.Len(t, mems, 1)
	assert.Equal(t, "Живее в София", mems[0].Content)
	assert.Equal(t, "active", mems[0].Status)
	assert.EqualValues(t, 7, mems[0].Importance)
	assert.Contains(t, string(mems[0].Metadata.RawMessage), "web_search")
	assert.Contains(t, string(mems[0].Metadata.RawMessage), fmt.Sprintf(`"proposal_id": %d`, p.ID))
}

func TestApprovingAnAlreadyKnownMemoryIsNotAnError(t *testing.T) {
	e := newEnv(t, nil)
	first, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", "Обича кафе", 5, nil)
	require.NoError(t, err)
	_, err = e.svc.Approve(e.ctx, e.userID, first.ID)
	require.NoError(t, err)

	again, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", "Обича кафе", 5, nil)
	require.NoError(t, err)
	_, err = e.svc.Approve(e.ctx, e.userID, again.ID)
	require.NoError(t, err, "the memory exists already; the user's yes is still a yes")
	assert.Len(t, e.memories(t, e.userID), 1)
}

func TestSameOpenProposalIsStoredOnce(t *testing.T) {
	e := newEnv(t, nil)

	a, err := e.svc.ProposeRule(e.ctx, e.userID, "s1", "language", "Говори на български", nil)
	require.NoError(t, err)
	b, err := e.svc.ProposeRule(e.ctx, e.userID, "s2", "language", "Говори на български", nil)
	require.NoError(t, err)
	assert.Equal(t, a.ID, b.ID, "a regenerated answer must not stack the same question")

	other, err := e.svc.ProposeRule(e.ctx, e.userID, "s1", "language", "Говори на английски", nil)
	require.NoError(t, err)
	assert.NotEqual(t, a.ID, other.ID)

	pending, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Len(t, pending, 2)

	// After a decision the same text can be proposed again.
	_, err = e.svc.Reject(e.ctx, e.userID, a.ID)
	require.NoError(t, err)
	c, err := e.svc.ProposeRule(e.ctx, e.userID, "s1", "language", "Говори на български", nil)
	require.NoError(t, err)
	assert.NotEqual(t, a.ID, c.ID)
}

func TestProposalsBelongToTheirUser(t *testing.T) {
	e := newEnv(t, nil)
	stranger := e.newUser(t)
	p, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "k", "значение", nil)
	require.NoError(t, err)

	_, err = e.svc.Approve(e.ctx, stranger, p.ID)
	assert.ErrorIs(t, err, ErrNotFound, "another user's proposal looks like a missing one")
	_, err = e.svc.Reject(e.ctx, stranger, p.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = e.svc.Approve(e.ctx, e.userID, p.ID+1000000)
	assert.ErrorIs(t, err, ErrNotFound)

	assert.Empty(t, e.rules(t, stranger))
	assert.Empty(t, e.rules(t, e.userID), "the failed attempts approved nothing")

	theirs, err := e.svc.Pending(e.ctx, stranger)
	require.NoError(t, err)
	assert.Empty(t, theirs)
}

func TestOnlyOneOfTwoApprovalsApplies(t *testing.T) {
	e := newEnv(t, nil)
	p, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", "Обича джаз", 5, nil)
	require.NoError(t, err)

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := e.svc.Approve(e.ctx, e.userID, p.ID)
			results <- err
		}()
	}
	first, second := <-results, <-results

	ok, conflict := 0, 0
	for _, err := range []error{first, second} {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotPending):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, conflict)
	assert.Len(t, e.memories(t, e.userID), 1)
}

func TestFailedApplyReopensTheProposal(t *testing.T) {
	e := newEnv(t, &llm.MockEmbedderWithConfig{Err: errors.New("embedder is down")})
	p, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", "Обича чай", 5, nil)
	require.NoError(t, err)

	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotPending)

	pending, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the user can try again once the embedder is back")
	assert.Equal(t, p.ID, pending[0].ID)
	assert.Empty(t, e.memories(t, e.userID))

	// The embedder is back.
	e.svc.retrieval = retrieval.NewRetrievalService(e.database, &llm.MockEmbedder{})
	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)
	assert.Len(t, e.memories(t, e.userID), 1)
}

func TestOpenProposalsAreCapped(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < MaxPending; i++ {
		_, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", fmt.Sprintf("факт номер %d", i), 5, nil)
		require.NoError(t, err)
	}

	_, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", "един факт отгоре", 5, nil)
	assert.ErrorIs(t, err, ErrTooManyPending)

	// Deciding one makes room.
	pending, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	_, err = e.svc.Reject(e.ctx, e.userID, pending[0].ID)
	require.NoError(t, err)
	_, err = e.svc.ProposeMemory(e.ctx, e.userID, "s", "един факт отгоре", 5, nil)
	assert.NoError(t, err)
}

func TestUnansweredProposalsExpire(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < MaxPending; i++ {
		_, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", fmt.Sprintf("стар факт %d", i), 5, nil)
		require.NoError(t, err)
	}
	_, err := e.database.Pool.Exec(e.ctx,
		"UPDATE pending_actions SET created_at = now() - $1::interval WHERE user_id = $2",
		fmt.Sprintf("%d hours", int(PendingTTL.Hours())+1), e.userID)
	require.NoError(t, err)

	listed, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Empty(t, listed, "expired proposals are not offered any more")

	_, err = e.svc.ProposeMemory(e.ctx, e.userID, "s", "нов факт", 5, nil)
	require.NoError(t, err, "expired proposals do not count against the limit")

	var left int
	require.NoError(t, e.database.Pool.QueryRow(e.ctx,
		"SELECT count(*) FROM pending_actions WHERE user_id = $1", e.userID).Scan(&left))
	assert.Equal(t, 1, left)
}

func TestRuleLimit(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < MaxActiveRules; i++ {
		p, err := e.svc.ProposeRule(e.ctx, e.userID, "s", fmt.Sprintf("rule%d", i), "стойност", nil)
		require.NoError(t, err)
		_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
		require.NoError(t, err)
	}

	_, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "one-too-many", "стойност", nil)
	assert.ErrorIs(t, err, ErrTooManyRules)

	// Replacing an existing rule is not a new rule.
	p, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "rule0", "нова стойност", nil)
	require.NoError(t, err)
	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)
	assert.Contains(t, e.rules(t, e.userID), "нова стойност")
	assert.Len(t, e.rules(t, e.userID), MaxActiveRules)
}

func TestRuleLimitIsCheckedAgainAtApproval(t *testing.T) {
	e := newEnv(t, nil)
	p, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "late", "стойност", nil)
	require.NoError(t, err)

	// The rules fill up while the proposal waits.
	for i := 0; i < MaxActiveRules; i++ {
		require.NoError(t, e.database.Queries.SaveGlobalRule(e.ctx, db.SaveGlobalRuleParams{
			UserID: e.userID, Kind: RuleKindRule, Key: fmt.Sprintf("other%d", i), Value: "x",
		}))
	}

	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	assert.ErrorIs(t, err, ErrTooManyRules)

	pending, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Len(t, pending, 1, "the proposal stays open so the user can delete a rule and approve again")
}

func TestUnreadablePayloadIsSkippedInTheList(t *testing.T) {
	e := newEnv(t, nil)
	good, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "k", "значение", nil)
	require.NoError(t, err)
	_, err = e.database.Pool.Exec(e.ctx,
		`INSERT INTO pending_actions (user_id, kind, payload, payload_hash) VALUES ($1, 'save_global_rule', '"not an object"', 'broken')`,
		e.userID)
	require.NoError(t, err)

	listed, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, good.ID, listed[0].ID)
}

func TestPendingIsOldestFirst(t *testing.T) {
	e := newEnv(t, nil)
	var ids []int64
	for i := 0; i < 3; i++ {
		p, err := e.svc.ProposeRule(e.ctx, e.userID, "s", fmt.Sprintf("k%d", i), "v", nil)
		require.NoError(t, err)
		ids = append(ids, p.ID)
		time.Sleep(5 * time.Millisecond)
	}
	listed, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, listed, 3)
	for i, p := range listed {
		assert.Equal(t, ids[i], p.ID)
	}
}

func (e *env) styleNotes(t *testing.T, userID uuid.UUID) []string {
	t.Helper()
	notes, err := e.database.Queries.GetStyleNotes(e.ctx, userID)
	require.NoError(t, err)
	return notes
}

// A note on how to talk waits for the user like a rule, and once confirmed it is a
// note, not a rule: the prompt shows each in its own section (plan item P42).
func TestProposedStyleNoteBecomesANoteWhenApproved(t *testing.T) {
	e := newEnv(t, nil)

	p, err := e.svc.ProposeStyleNote(e.ctx, e.userID, "sess", "humour", "Шегува се — отговаряй с шега", nil)
	require.NoError(t, err)
	assert.Equal(t, KindSaveStyleNote, p.Kind)
	assert.Equal(t, "Как говорим: Шегува се — отговаряй с шега", p.Summary)
	assert.Empty(t, e.styleNotes(t, e.userID), "a proposal must not change the prompt")

	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Шегува се — отговаряй с шега"}, e.styleNotes(t, e.userID))
	assert.Empty(t, e.rules(t, e.userID), "a note is not a rule")
}

// A rule and a note may share a key: approving one does not replace the other.
func TestRuleAndStyleNoteWithTheSameKeyBothStay(t *testing.T) {
	e := newEnv(t, nil)
	for _, propose := range []func() (Proposal, error){
		func() (Proposal, error) {
			return e.svc.ProposeRule(e.ctx, e.userID, "s", "humour", "правило", nil)
		},
		func() (Proposal, error) {
			return e.svc.ProposeStyleNote(e.ctx, e.userID, "s", "humour", "бележка", nil)
		},
	} {
		p, err := propose()
		require.NoError(t, err)
		_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"правило"}, e.rules(t, e.userID))
	assert.Equal(t, []string{"бележка"}, e.styleNotes(t, e.userID))
}

// The notes have a limit of their own: full notes do not stop a rule, and full rules
// do not stop a note.
func TestStyleNoteLimitIsSeparateFromTheRules(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < MaxStyleNotes; i++ {
		p, err := e.svc.ProposeStyleNote(e.ctx, e.userID, "s", fmt.Sprintf("note%d", i), "стойност", nil)
		require.NoError(t, err)
		_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
		require.NoError(t, err)
	}
	_, err := e.svc.ProposeStyleNote(e.ctx, e.userID, "s", "one-too-many", "стойност", nil)
	assert.ErrorIs(t, err, ErrTooManyStyleNotes)

	_, err = e.svc.ProposeRule(e.ctx, e.userID, "s", "a-rule", "стойност", nil)
	assert.NoError(t, err, "the rules have room of their own")

	// Replacing an existing note is not a new one.
	p, err := e.svc.ProposeStyleNote(e.ctx, e.userID, "s", "note0", "нова стойност", nil)
	require.NoError(t, err)
	_, err = e.svc.Approve(e.ctx, e.userID, p.ID)
	require.NoError(t, err)
	assert.Len(t, e.styleNotes(t, e.userID), MaxStyleNotes)
	assert.Contains(t, e.styleNotes(t, e.userID), "нова стойност")
}
