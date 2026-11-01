package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
)

// summaryLLM is the model of the summarising pass: it records what it was asked
// and answers with a canned text. Streaming (the chat itself) is delegated to
// the embedded mock.
type summaryLLM struct {
	*llm.MockLLM

	mu         sync.Mutex
	answer     string
	err        error
	gate       chan struct{} // when set, Chat waits for it to be closed
	prompts    []string
	requests   []openai.ChatCompletionRequest
	background []bool
}

func (s *summaryLLM) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	s.mu.Lock()
	// "The prompt" is everything the model was shown: the instructions (system)
	// and the conversation to digest (user).
	var shown []string
	for _, m := range req.Messages {
		shown = append(shown, m.Content)
	}
	s.prompts = append(s.prompts, strings.Join(shown, "\n"))
	s.requests = append(s.requests, req)
	s.background = append(s.background, llm.IsBackground(ctx))
	gate, failure := s.gate, s.err
	s.mu.Unlock()

	if gate != nil {
		<-gate
	}
	if failure != nil {
		return openai.ChatCompletionResponse{}, failure
	}
	s.mu.Lock()
	answer := s.answer
	s.mu.Unlock()
	return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{
		Message: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: answer},
	}}}, nil
}

func (s *summaryLLM) setGate(gate chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = gate
}

func (s *summaryLLM) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

func (s *summaryLLM) prompt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompts[i]
}

func (s *summaryLLM) setFailure(err error, answer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err, s.answer = err, answer
}

// summaryEnv is a disposable user with a real database behind the orchestrator.
type summaryEnv struct {
	o      *Orchestrator
	db     *db.Database
	model  *summaryLLM
	userID uuid.UUID
	base   time.Time
	ctx    context.Context
}

func newSummaryEnv(t *testing.T) *summaryEnv {
	t.Helper()

	connString := dbtest.ConnString(t)
	// db.RunMigrations resolves "migrations" relative to the working directory.
	t.Chdir("../..")

	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(connString))
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	// Only this test's own user is removed; the cascade takes its messages and
	// summaries with it.
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })

	model := &summaryLLM{
		MockLLM: &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Отговор")}},
		answer:  "резюме-текст",
	}
	retrievalService := retrieval.NewRetrievalService(database, &llm.MockEmbedder{})

	return &summaryEnv{
		o:      NewOrchestrator(database, retrievalService, model, &MockToolRegistry{}),
		db:     database,
		model:  model,
		userID: user.ID,
		base:   time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		ctx:    ctx,
	}
}

// at is the creation time of message number i.
func (e *summaryEnv) at(i int) time.Time { return e.base.Add(time.Duration(i) * time.Second) }

// addMessages stores the messages from..to-1 of a session, alternating user and
// assistant, each marked "[#i]" so a test can tell which ones a prompt contains.
func (e *summaryEnv) addMessages(t *testing.T, userID uuid.UUID, session string, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		role := openai.ChatMessageRoleUser
		if i%2 == 1 {
			role = openai.ChatMessageRoleAssistant
		}
		_, err := e.db.SqlDB.ExecContext(e.ctx,
			`INSERT INTO messages (user_id, session_id, role, content, created_at) VALUES ($1, $2, $3, $4, $5)`,
			userID, session, role, fmt.Sprintf("съобщение [#%d]", i), e.at(i))
		require.NoError(t, err)
	}
}

// summarise asks for the summary the way a chat turn does and waits for any
// pass that turn started.
func (e *summaryEnv) summarise(session string, promptStart time.Time) string {
	summary := e.o.rollingSummary(e.ctx, e.userID, session, promptStart)
	e.o.summaryWG.Wait()
	return summary
}

func (e *summaryEnv) stored(t *testing.T, userID uuid.UUID, session string) (db.SessionSummary, bool) {
	t.Helper()
	row, err := e.db.Queries.GetSessionSummary(e.ctx, db.GetSessionSummaryParams{UserID: userID, SessionID: session})
	if err == sql.ErrNoRows {
		return db.SessionSummary{}, false
	}
	require.NoError(t, err)
	return row, true
}

func (e *summaryEnv) messageID(t *testing.T, session string, i int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, e.db.SqlDB.QueryRowContext(e.ctx,
		`SELECT id FROM messages WHERE user_id = $1 AND session_id = $2 AND content = $3`,
		e.userID, session, fmt.Sprintf("съобщение [#%d]", i)).Scan(&id))
	return id
}

func requireSameInstant(t *testing.T, want, got time.Time, msg string) {
	t.Helper()
	require.True(t, want.Equal(got), "%s: want %s, got %s", msg, want, got)
}

// TestRollingSummaryCoversTheOldMessagesOnce is the scenario of the task: after
// 40 messages, with the prompt holding the last 10, the summary covers the first
// 30 - and asking again does not summarise them a second time.
func TestRollingSummaryCoversTheOldMessagesOnce(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 40)

	require.Equal(t, "", e.summarise("sess", e.at(30)), "nothing is summarised yet when the turn starts")

	require.Equal(t, 1, e.model.calls())
	first := e.model.prompt(0)
	assert.Contains(t, first, "[#0]")
	assert.Contains(t, first, "[#29]")
	assert.NotContains(t, first, "[#30]", "the messages of the prompt are not summarised")

	row, ok := e.stored(t, e.userID, "sess")
	require.True(t, ok)
	assert.Equal(t, "резюме-текст", row.Summary)
	requireSameInstant(t, e.at(29), row.CoveredUntilAt, "covered until")
	assert.Equal(t, e.messageID(t, "sess", 29), row.CoveredUntilMessageID)

	// The next turn reads the stored summary and finds nothing new to summarise.
	assert.Equal(t, "резюме-текст", e.summarise("sess", e.at(30)))
	assert.Equal(t, 1, e.model.calls(), "the same messages are not summarised twice")
}

func TestRollingSummaryRunsAsBackgroundWorkWithoutThinking(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 20)

	e.summarise("sess", e.at(10))

	require.Equal(t, 1, e.model.calls())
	assert.True(t, e.model.background[0], "a summary must not take a chat slot")
	req := e.model.requests[0]
	assert.Equal(t, map[string]any{"enable_thinking": false}, req.ChatTemplateKwargs)
	assert.Positive(t, req.MaxTokens)

	// The instructions are a system message and the conversation a user message:
	// a chat template without a user turn has nothing to answer.
	require.Len(t, req.Messages, 2)
	assert.Equal(t, openai.ChatMessageRoleSystem, req.Messages[0].Role)
	assert.Equal(t, openai.ChatMessageRoleUser, req.Messages[1].Role)
	assert.NotContains(t, req.Messages[0].Content, "[#", "the conversation is not part of the instructions")
	assert.Contains(t, req.Messages[1].Content, "[#0]")
}

func TestRollingSummarySurvivesARestart(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 40)
	e.summarise("sess", e.at(30))
	require.Equal(t, 1, e.model.calls())

	// A new process: nothing in memory, only the database.
	restarted := NewOrchestrator(e.db, nil, e.model, nil)
	summary := restarted.rollingSummary(e.ctx, e.userID, "sess", e.at(30))
	restarted.summaryWG.Wait()

	assert.Equal(t, "резюме-текст", summary, "the summary is there from the first turn after the restart")
	assert.Equal(t, 1, e.model.calls(), "and nothing is summarised again")
}

func TestRollingSummaryContinuesWhereItStopped(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 40)
	e.summarise("sess", e.at(30))

	// Ten more messages: the prompt now starts at #40 and #30-#39 have left it.
	e.addMessages(t, e.userID, "sess", 40, 50)
	e.model.setFailure(nil, "резюме-две")
	assert.Equal(t, "резюме-текст", e.summarise("sess", e.at(40)), "the turn that starts the pass still sees the old summary")

	require.Equal(t, 2, e.model.calls())
	second := e.model.prompt(1)
	assert.Contains(t, second, "резюме-текст", "the previous summary is carried into the next one")
	assert.Contains(t, second, "[#30]")
	assert.Contains(t, second, "[#39]")
	assert.NotContains(t, second, "[#29]", "what the summary already covers is not read again")
	assert.NotContains(t, second, "[#40]")

	row, _ := e.stored(t, e.userID, "sess")
	assert.Equal(t, "резюме-две", row.Summary)
	requireSameInstant(t, e.at(39), row.CoveredUntilAt, "covered until")
}

// TestRollingSummaryStartsOnePassPerSession: two turns of the same session in
// quick succession must not both summarise the same messages.
func TestRollingSummaryStartsOnePassPerSession(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 20)

	gate := make(chan struct{})
	e.model.setGate(gate)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		e.o.summaryWG.Wait()
	})

	e.o.rollingSummary(e.ctx, e.userID, "sess", e.at(10))
	require.Eventually(t, func() bool { return e.model.calls() == 1 }, 3*time.Second, time.Millisecond)

	// The first pass is still running, held at the gate.
	e.o.rollingSummary(e.ctx, e.userID, "sess", e.at(10))
	require.Never(t, func() bool { return e.model.calls() > 1 }, 200*time.Millisecond, 5*time.Millisecond,
		"a second pass for the same session started while the first one was running")

	close(gate)
	e.o.summaryWG.Wait()
	assert.Equal(t, 1, e.model.calls())
}

func TestRollingSummaryWaitsForEnoughMessages(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 8)

	// Five messages are outside the prompt: fewer than summaryMinNewMessages.
	require.Equal(t, "", e.summarise("sess", e.at(5)))
	assert.Zero(t, e.model.calls())
	_, ok := e.stored(t, e.userID, "sess")
	assert.False(t, ok)

	// One more leaves the prompt and the threshold is reached.
	e.summarise("sess", e.at(6))
	assert.Equal(t, 1, e.model.calls())
}

func TestRollingSummaryWorksThroughABacklogOldestFirst(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 110)

	e.summarise("sess", e.at(100))
	require.Equal(t, 1, e.model.calls())
	assert.Contains(t, e.model.prompt(0), "[#0]")
	assert.Contains(t, e.model.prompt(0), fmt.Sprintf("[#%d]", maxSummaryInputMessages-1))
	assert.NotContains(t, e.model.prompt(0), fmt.Sprintf("[#%d]", maxSummaryInputMessages), "one pass reads a bounded number of messages")

	row, _ := e.stored(t, e.userID, "sess")
	requireSameInstant(t, e.at(maxSummaryInputMessages-1), row.CoveredUntilAt, "first pass")

	// The following pass takes up exactly where the first stopped: no message is
	// skipped and none is read twice.
	e.summarise("sess", e.at(100))
	require.Equal(t, 2, e.model.calls())
	assert.NotContains(t, e.model.prompt(1), fmt.Sprintf("[#%d]", maxSummaryInputMessages-1))
	assert.Contains(t, e.model.prompt(1), fmt.Sprintf("[#%d]", maxSummaryInputMessages))
}

// TestRollingSummaryOnlyCoversTheMessagesItActuallyRead: a pass has a token
// budget. Messages that did not fit into it must stay pending, not be marked as
// summarised, or they would silently vanish from the conversation.
func TestRollingSummaryOnlyCoversTheMessagesItActuallyRead(t *testing.T) {
	e := newSummaryEnv(t)
	for i := 0; i < 30; i++ {
		// ~900 tokens each, cut to maxSummaryMessageTokens (500) in the transcript.
		_, err := e.db.SqlDB.ExecContext(e.ctx,
			`INSERT INTO messages (user_id, session_id, role, content, created_at) VALUES ($1, 'sess', 'user', $2, $3)`,
			e.userID, fmt.Sprintf("[#%d] %s", i, strings.Repeat("д", 1800)), e.at(i))
		require.NoError(t, err)
	}

	e.summarise("sess", e.at(25))

	require.Equal(t, 1, e.model.calls())
	prompt := e.model.prompt(0)
	read := 0
	for i := 0; i < 25; i++ {
		if strings.Contains(prompt, fmt.Sprintf("[#%d]", i)) {
			read++
		}
	}
	require.Greater(t, read, 0)
	require.Less(t, read, 25, "the transcript budget cuts the pass short")

	row, ok := e.stored(t, e.userID, "sess")
	require.True(t, ok)
	requireSameInstant(t, e.at(read-1), row.CoveredUntilAt, "covered exactly the messages that were read")
}

func TestRollingSummaryDoesNotDependOnHowMuchHistoryThePromptReads(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 200)

	// The old rule saw only the newest rows the prompt read; the position is
	// now a time, so the first message of a long session is still found.
	e.summarise("sess", e.at(190))

	require.Equal(t, 1, e.model.calls())
	assert.Contains(t, e.model.prompt(0), "[#0]")
}

func TestRollingSummaryFailureLeavesTheMessagesPending(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 20)
	e.model.setFailure(fmt.Errorf("llm unavailable"), "")

	require.Equal(t, "", e.summarise("sess", e.at(10)))

	_, ok := e.stored(t, e.userID, "sess")
	require.False(t, ok, "a failed pass records nothing")
	require.True(t, e.o.summaryRuns.begin(summaryKey(e.userID, "sess")),
		"the in-flight marker is cleared, or the session would never be summarised again")
	e.o.summaryRuns.end(summaryKey(e.userID, "sess"))

	e.model.setFailure(nil, "резюме-текст")
	e.summarise("sess", e.at(10))
	row, ok := e.stored(t, e.userID, "sess")
	require.True(t, ok, "the next turn tries again with the same messages")
	requireSameInstant(t, e.at(9), row.CoveredUntilAt, "covered until")
}

func TestRollingSummaryIgnoresAnEmptyAnswer(t *testing.T) {
	for name, answer := range map[string]string{
		"blank":         "   ",
		"only thinking": "<think>няма какво да кажа</think>",
	} {
		t.Run(name, func(t *testing.T) {
			e := newSummaryEnv(t)
			e.addMessages(t, e.userID, "sess", 0, 20)
			e.model.setFailure(nil, answer)

			e.summarise("sess", e.at(10))

			_, ok := e.stored(t, e.userID, "sess")
			assert.False(t, ok, "covering the messages with an empty summary would lose them")
		})
	}
}

func TestRollingSummaryHidesThinkingFromTheStoredText(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 20)
	e.model.setFailure(nil, "<think>първо ще помисля</think>\nпотребителят обича чай")

	e.summarise("sess", e.at(10))

	row, ok := e.stored(t, e.userID, "sess")
	require.True(t, ok)
	assert.Equal(t, "потребителят обича чай", row.Summary)
}

func TestRollingSummaryIsScopedPerUser(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "shared-session", 0, 20)
	e.summarise("shared-session", e.at(10))

	other, err := e.db.Queries.CreateUser(e.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, e.db.Pool, other.ID) })

	// Same session id, different owner: nothing of the first user's summary.
	summary := e.o.rollingSummary(e.ctx, other.ID, "shared-session", e.at(10))
	e.o.summaryWG.Wait()

	assert.Equal(t, "", summary)
	_, ok := e.stored(t, other.ID, "shared-session")
	assert.False(t, ok)
	assert.Equal(t, 1, e.model.calls(), "the other user has no messages, so there is nothing to summarise")
}

func TestUpsertSessionSummaryNeverMovesTheSummaryBack(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 3)
	newer, older := e.messageID(t, "sess", 2), e.messageID(t, "sess", 0)

	save := func(text string, id uuid.UUID, at time.Time) {
		require.NoError(t, e.db.Queries.UpsertSessionSummary(e.ctx, db.UpsertSessionSummaryParams{
			UserID: e.userID, SessionID: "sess", Summary: text, CoveredUntilMessageID: id, CoveredUntilAt: at,
		}))
	}
	save("новo", newer, e.at(2))
	save("старо", older, e.at(0)) // a pass that finished late

	row, _ := e.stored(t, e.userID, "sess")
	assert.Equal(t, "новo", row.Summary)
	assert.Equal(t, newer, row.CoveredUntilMessageID)
	requireSameInstant(t, e.at(2), row.CoveredUntilAt, "position")
}

// TestChatTurnSummarisesTheOldMessagesAndUsesThemNextTurn walks the real path:
// 39 messages in the database, a chat turn makes the 40th, the prompt reads the
// newest ten, and the first thirty end up in the summary - which the next turn's
// system prompt contains.
func TestChatTurnSummarisesTheOldMessagesAndUsesThemNextTurn(t *testing.T) {
	e := newSummaryEnv(t)
	e.addMessages(t, e.userID, "sess", 0, 39)

	_, err := e.o.ProcessChat(e.ctx, e.userID, ChatRequest{SessionID: "sess", Content: "Първи въпрос след дълъг разговор"})
	require.NoError(t, err)
	e.o.summaryWG.Wait()

	require.Equal(t, 1, e.model.calls())
	row, ok := e.stored(t, e.userID, "sess")
	require.True(t, ok)
	requireSameInstant(t, e.at(29), row.CoveredUntilAt, "the first thirty messages are covered")

	_, err = e.o.ProcessChat(e.ctx, e.userID, ChatRequest{SessionID: "sess", Content: "Втори въпрос"})
	require.NoError(t, err)
	e.o.summaryWG.Wait()

	requests := e.model.MockLLM.Requests
	require.Len(t, requests, 2)
	assert.NotContains(t, requests[0].Messages[0].Content, "резюме-текст", "the first turn had no summary yet")
	assert.Contains(t, requests[1].Messages[0].Content, "резюме-текст", "the second turn has it in the system prompt")
	assert.True(t, strings.Contains(requests[1].Messages[0].Content, "Обобщение на предишни съобщения"))
}
