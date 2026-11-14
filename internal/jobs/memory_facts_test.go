package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/prompts"
)

func memoryWithValue(status, value string) db.Memory {
	meta, _ := json.Marshal(map[string]string{"fact_key": "location", "value": value})
	return db.Memory{
		ID:       uuid.New(),
		Status:   status,
		Metadata: pqtype.NullRawMessage{RawMessage: meta, Valid: true},
	}
}

func ids(ms ...db.Memory) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestDecideFact(t *testing.T) {
	sofia := memoryWithValue("active", "София")
	varnaCandidate := memoryWithValue("candidate", "Варна")
	burgasCandidate := memoryWithValue("candidate", "Бургас")

	t.Run("nothing stored yet", func(t *testing.T) {
		got := decideFact(Fact{FactKey: "location", Value: "София"}, nil, nil)
		assert.Equal(t, factAction{store: true, status: "active"}, got)
	})
	t.Run("the same statement is already active", func(t *testing.T) {
		got := decideFact(Fact{FactKey: "location", Value: "София"}, &sofia, []db.Memory{sofia})
		assert.False(t, got.store)
	})
	t.Run("explicit change replaces what it contradicts", func(t *testing.T) {
		got := decideFact(Fact{FactKey: "location", Value: "Варна", IsExplicitChange: true}, nil, []db.Memory{sofia, burgasCandidate})
		assert.True(t, got.store)
		assert.Equal(t, "active", got.status)
		assert.ElementsMatch(t, ids(sofia, burgasCandidate), got.supersede)
	})
	t.Run("a silent contradiction is only a candidate", func(t *testing.T) {
		got := decideFact(Fact{FactKey: "location", Value: "Варна"}, nil, []db.Memory{sofia})
		assert.True(t, got.store)
		assert.Equal(t, "candidate", got.status)
		assert.Empty(t, got.supersede, "the active fact stays in force")
	})
	t.Run("a newer silent claim replaces the older candidate, not the active fact", func(t *testing.T) {
		got := decideFact(Fact{FactKey: "location", Value: "Пловдив"}, nil, []db.Memory{sofia, burgasCandidate})
		assert.Equal(t, "candidate", got.status)
		assert.Equal(t, ids(burgasCandidate), got.supersede)
	})
	t.Run("the same statement again confirms the candidate", func(t *testing.T) {
		got := decideFact(Fact{FactKey: "location", Value: "Варна"}, &varnaCandidate, []db.Memory{sofia, varnaCandidate})
		assert.True(t, got.store)
		assert.Equal(t, "active", got.status)
		assert.Equal(t, ids(sofia), got.supersede, "only the contradicted fact, not the candidate itself")
	})
	t.Run("the same value in other words confirms it too", func(t *testing.T) {
		got := decideFact(Fact{FactKey: "location", Value: " варна "}, nil, []db.Memory{sofia, varnaCandidate})
		assert.Equal(t, "active", got.status)
		assert.ElementsMatch(t, ids(sofia, varnaCandidate), got.supersede)
	})
	t.Run("a replaced statement said again comes back", func(t *testing.T) {
		old := memoryWithValue("superseded", "София")
		varna := memoryWithValue("active", "Варна")
		got := decideFact(Fact{FactKey: "location", Value: "София", IsExplicitChange: true}, &old, []db.Memory{varna})
		assert.True(t, got.store)
		assert.Equal(t, "active", got.status)
		assert.Equal(t, ids(varna), got.supersede)
	})
	t.Run("a fact without a key never conflicts", func(t *testing.T) {
		got := decideFact(Fact{Value: "пица"}, nil, nil)
		assert.Equal(t, "active", got.status)
	})
}

// consolidationEnv runs the worker against the test database with a scripted
// model: every analysed message consumes the next response.
type consolidationEnv struct {
	t        *testing.T
	ctx      context.Context
	database *db.Database
	user     db.User
	llm      *mockLLMClient
	worker   *MemoryConsolidationWorker
}

func newConsolidationEnv(t *testing.T, responses ...string) *consolidationEnv {
	t.Helper()
	dbURL := dbtest.ConnString(t)
	t.Chdir("../..")
	prompts.Init("prompts")

	ctx := context.Background()
	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	llmMock := &mockLLMClient{responses: responses}
	return &consolidationEnv{
		t: t, ctx: ctx, database: database, user: user, llm: llmMock,
		worker: NewMemoryConsolidationWorker(database, llmMock, &mockEmbedder{}),
	}
}

// say stores a user message and lets the worker analyse it.
func (e *consolidationEnv) say(text string) {
	e.t.Helper()
	_, err := e.database.Queries.CreateMessage(e.ctx, db.CreateMessageParams{
		UserID: e.user.ID, SessionID: "s", Role: "user", Content: text,
	})
	require.NoError(e.t, err)
	require.NoError(e.t, e.worker.Work(e.ctx, &river.Job[MemoryConsolidationArgs]{Args: MemoryConsolidationArgs{UserID: e.user.ID}}))
}

// statuses returns statement -> status of every stored memory.
func (e *consolidationEnv) statuses() map[string]string {
	e.t.Helper()
	mems, err := e.database.Queries.GetMemories(e.ctx, db.GetMemoriesParams{UserID: e.user.ID})
	require.NoError(e.t, err)
	out := map[string]string{}
	for _, m := range mems {
		out[m.Content] = m.Status
	}
	require.Len(e.t, out, len(mems), "two memories with one statement")
	return out
}

func factJSON(key, value, statement string, explicit bool) string {
	return fmt.Sprintf(`[{"fact_key": %q, "value": %q, "statement": %q, "is_explicit_change": %v}]`, key, value, statement, explicit)
}

const (
	inSofia = "Живея в София"
	inVarna = "Живея във Варна"
)

// The same fact in a second message used to be stored again: the content hash
// contained the time, so the unique index never matched.
func TestConsolidationDoesNotDuplicateAFact(t *testing.T) {
	e := newConsolidationEnv(t,
		factJSON("location", "София", inSofia, false),
		factJSON("location", "София", inSofia, false),
		factJSON("location", "София", "  живея в софия  ", false), // same fact, different spelling
	)
	e.say("Живея в София")
	e.say("Да, живея в София")
	e.say("Пак: живея в София")

	assert.Equal(t, map[string]string{inSofia: "active"}, e.statuses())
}

// A contradiction that is not an explicit change used to be promoted to active
// by the next run, leaving two facts that exclude each other both active.
func TestConsolidationKeepsASilentContradictionAsCandidate(t *testing.T) {
	e := newConsolidationEnv(t,
		factJSON("location", "София", inSofia, false),
		factJSON("location", "Варна", inVarna, false),
		`[]`, `[]`, // later messages with nothing to remember: more runs
	)
	e.say("Живея в София")
	e.say("Живея във Варна")
	assert.Equal(t, map[string]string{inSofia: "active", inVarna: "candidate"}, e.statuses())

	e.say("Как си?")
	e.say("Добре съм")
	assert.Equal(t, map[string]string{inSofia: "active", inVarna: "candidate"}, e.statuses(),
		"another run must not promote the candidate")
}

func TestConsolidationConfirmationPromotesTheCandidate(t *testing.T) {
	e := newConsolidationEnv(t,
		factJSON("location", "София", inSofia, false),
		factJSON("location", "Варна", inVarna, false),
		factJSON("location", "Варна", inVarna, false), // said again
	)
	e.say("Живея в София")
	e.say("Живея във Варна")
	e.say("Да, във Варна живея")

	assert.Equal(t, map[string]string{inVarna: "active"}, e.statuses(), "the replaced fact is deleted")
}

// A replaced fact is deleted; said again, it is stored anew and replaces the one
// in force (whose own row then goes).
func TestConsolidationReplacedFactCanComeBack(t *testing.T) {
	e := newConsolidationEnv(t,
		factJSON("location", "София", inSofia, false),
		factJSON("location", "Варна", inVarna, true),
		factJSON("location", "София", inSofia, true),
	)
	e.say("Живея в София")
	e.say("Преместих се във Варна")
	assert.Equal(t, map[string]string{inVarna: "active"}, e.statuses())

	e.say("Върнах се в София")
	assert.Equal(t, map[string]string{inSofia: "active"}, e.statuses())
}

// A row an older version left as "superseded" still has its hash in the unique
// index: said again, the fact must bring that row back, not fail on a new insert.
func TestConsolidationRevivesARowLeftAsSuperseded(t *testing.T) {
	e := newConsolidationEnv(t,
		factJSON("location", "Варна", inVarna, false),
		factJSON("location", "София", inSofia, true),
	)
	e.say("Живея във Варна")
	_, err := e.database.SqlDB.ExecContext(e.ctx, `INSERT INTO memories (user_id, content, status, embedding, embedding_model, importance, metadata, content_hash)
		VALUES ($1, $2, 'superseded', $3, 'x', 1, '{"fact_key": "location", "value": "София"}', $4)`,
		e.user.ID, inSofia, pgvector.NewVector(make([]float32, config.VectorDimension)), db.GenerateHash(inSofia))
	require.NoError(t, err)

	e.say("Върнах се в София")
	assert.Equal(t, map[string]string{inSofia: "active"}, e.statuses())
}

// A newer unconfirmed claim replaces an older candidate for the same key, and the
// older candidate is deleted with it; the active fact stays.
func TestConsolidationDeletesTheCandidateANewerClaimReplaces(t *testing.T) {
	const inBurgas = "Живея в Бургас"
	e := newConsolidationEnv(t,
		factJSON("location", "София", inSofia, false),
		factJSON("location", "Варна", inVarna, false),
		factJSON("location", "Бургас", inBurgas, false),
	)
	e.say("Живея в София")
	e.say("Живея във Варна")
	e.say("Живея в Бургас")
	assert.Equal(t, map[string]string{inSofia: "active", inBurgas: "candidate"}, e.statuses())
}

// The model gets its instructions as a system message, not as one more thing
// the user said.
func TestConsolidationSendsInstructionsAsSystemMessage(t *testing.T) {
	var seen []string
	e := newConsolidationEnv(t)
	e.worker.llmClient = &roleRecordingLLM{roles: &seen}
	e.say("Живея в София")

	require.NotEmpty(t, seen)
	assert.Equal(t, []string{"system", "user"}, seen)
}

// roleRecordingLLM remembers the roles of the messages it is asked about.
type roleRecordingLLM struct {
	mockLLMClient
	roles *[]string
}

func (r *roleRecordingLLM) ChatWithJSONSchema(_ context.Context, req openai.ChatCompletionRequest, _ interface{}) (openai.ChatCompletionResponse, error) {
	for _, m := range req.Messages {
		*r.roles = append(*r.roles, m.Role)
	}
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: "[]"}}},
	}, nil
}
