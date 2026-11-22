package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
)

var windowStart = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

func msgAt(session, role, content string, minute int) db.Message {
	return db.Message{ID: uuid.New(), SessionID: session, Role: role, Content: content,
		CreatedAt: sql.NullTime{Time: windowStart.Add(time.Duration(minute) * time.Minute), Valid: true}}
}

// talk is a conversation of n messages of the person, each answered, one a minute.
func talk(session string, n, from int) []db.Message {
	var out []db.Message
	for i := 0; i < n; i++ {
		out = append(out, msgAt(session, "user", fmt.Sprintf("съобщение %d", i), from+2*i), msgAt(session, "assistant", "отговор", from+2*i+1))
	}
	return out
}

func TestTheAnalysisWaitsForAQuietConversationAndReadsALongOneInWindows(t *testing.T) {
	long := talk("a", 6, 0)   // 12 messages, the last at minute 11
	short := talk("b", 2, 0)  // 4 messages
	busy := talk("c", 6, 100) // the last at minute 111
	all := append(append(append([]db.Message{}, long...), short...), busy...)

	plan := planAnalysis(all, windowStart.Add(40*time.Minute))
	require.Len(t, plan.windows, 1)
	assert.Equal(t, long, plan.windows[0], "a quiet conversation with 5 or more messages of the person is one window")
	assert.Equal(t, short, plan.single, "a shorter one is read message by message, as before")
	assert.Equal(t, len(busy), plan.waiting, "a conversation that is not quiet for 20 minutes waits")

	plan = planAnalysis(all, windowStart.Add(120*time.Minute))
	assert.Equal(t, len(busy), plan.waiting, "its last message is 9 minutes old")

	plan = planAnalysis(all, windowStart.Add(200*time.Minute))
	assert.Len(t, plan.windows, 2)
	assert.Zero(t, plan.waiting)
}

func TestAWindowEndsAtAPauseAndAtTwentyMessages(t *testing.T) {
	msgs := append(talk("a", 3, 0), talk("a", 3, 60)...) // a pause of 55 minutes between the two halves
	plan := planAnalysis(msgs, windowStart.Add(5*time.Hour))
	require.Len(t, plan.windows, 2)
	assert.Len(t, plan.windows[0], 6)
	assert.Len(t, plan.windows[1], 6)

	plan = planAnalysis(talk("b", 15, 0), windowStart.Add(5*time.Hour))
	require.Len(t, plan.windows, 2)
	assert.Len(t, plan.windows[0], windowMaxMessages)
	assert.Len(t, plan.windows[1], 30-windowMaxMessages)

	// A window that failed once is read message by message.
	retried := talk("c", 6, 0)
	retried[2].AnalysisAttempts = 1
	plan = planAnalysis(retried, windowStart.Add(5*time.Hour))
	assert.Empty(t, plan.windows)
	assert.Len(t, plan.single, 12)
}

func TestTheWindowShowsWhoSaidWhatAndFindsTheSourceOfAFact(t *testing.T) {
	w := []db.Message{
		msgAt("a", "user", "Здравей!", 0),
		msgAt("a", "assistant", "Здрасти. За теб като човек с търговска система…", 1),
		msgAt("a", "user", "Аз  съм от\nБългария, живея в Бремен.", 2),
		msgAt("a", "user", strings.Repeat("я", windowMaxRunes+10), 3),
	}
	conversation, own := windowText(w, "Ивайло")
	assert.Contains(t, conversation, "Ивайло: Здравей!\nЛили: Здрасти. За теб като човек с търговска система…\nИвайло: Аз съм от България, живея в Бремен.\n")
	assert.NotContains(t, own, "търговска система", "the evidence must be in the person's lines, not Lili's")
	assert.Contains(t, own, strings.Repeat("я", windowMaxRunes)+"…\n", "a long message is cut")

	assert.Equal(t, w[2].ID, sourceOf(w, Fact{Evidence: "аз съм от България"}).ID)
	assert.Equal(t, w[3].ID, sourceOf(w, Fact{Evidence: "нещо друго"}).ID, "else the last message of the person")
}

// Through the worker: a long quiet conversation is read as one window; a fact from Lili's words is
// dropped, at most three are kept, and every message of the window is marked read.
func TestTheWorkerReadsAWindowAndKeepsOnlyThePersonsFacts(t *testing.T) {
	answer := `[` +
		`{"evidence": "живея в Бремен", "fact_key": "location", "value": "Бремен", "statement": "Потребителят живее в Бремен", "is_explicit_change": false},` +
		`{"evidence": "човек с търговска система", "fact_key": "trading", "value": "система", "statement": "Потребителят има търговска система", "is_explicit_change": false},` +
		`{"evidence": "обичам шаха", "fact_key": "hobby", "value": "шах", "statement": "Потребителят обича шаха", "is_explicit_change": false},` +
		`{"evidence": "имам котка", "fact_key": "pet", "value": "котка", "statement": "Потребителят има котка", "is_explicit_change": false},` +
		`{"evidence": "пиша на Go", "fact_key": "language", "value": "Go", "statement": "Потребителят пише на Go", "is_explicit_change": false}]`
	e := newConsolidationEnv(t, answer)
	ctx := context.Background()
	lines := []struct{ role, content string }{
		{"user", "Здравей"}, {"assistant", "Здрасти. За теб като човек с търговска система…"},
		{"user", "живея в Бремен от години"}, {"assistant", "Хубав град."},
		{"user", "обичам шаха"}, {"user", "имам котка"}, {"user", "пиша на Go"}, {"user", "това е всичко"},
	}
	for _, l := range lines {
		_, err := e.database.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: e.user.ID, SessionID: "w", Role: l.role, Content: l.content})
		require.NoError(t, err)
	}
	require.NoError(t, e.worker.Work(ctx, &river.Job[MemoryConsolidationArgs]{Args: MemoryConsolidationArgs{UserID: e.user.ID}}))

	assert.Equal(t, map[string]string{
		"Потребителят живее в Бремен": "active",
		"Потребителят обича шаха":     "active",
		"Потребителят има котка":      "active",
	}, e.statuses(), "Lili's words are not the person's; three facts a window")
	require.Len(t, e.llm.requests, 1, "one call for the whole window")
	assert.Contains(t, e.llm.requests[0].Messages[1].Content, "Лили: Хубав град.")

	left, err := e.database.Queries.GetUnanalyzedMessages(ctx, e.user.ID)
	require.NoError(t, err)
	assert.Empty(t, left, "every message of the window is read")
	mems, err := e.database.Queries.GetMemories(ctx, db.GetMemoriesParams{UserID: e.user.ID})
	require.NoError(t, err)
	for _, m := range mems {
		require.True(t, m.SourceMessageID.Valid)
	}
}

// The memories already known are in the window prompt, so that the model does not return them
// again (measured: 0 repeats of a known memory with the list, about 4 without it).
func TestTheWindowPromptListsTheKnownMemories(t *testing.T) {
	e := newConsolidationEnv(t, `[]`)
	ctx := context.Background()
	_, err := e.database.Queries.CreateMemory(ctx, db.CreateMemoryParams{UserID: e.user.ID, Content: "Потребителят живее в Бремен.", Status: "active",
		Importance: 5, ContentHash: uuid.NewString(), EmbeddingModel: "x", Embedding: zeroVector()})
	require.NoError(t, err)
	for _, m := range talk("k", 5, 0) {
		_, err := e.database.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: e.user.ID, SessionID: "k", Role: m.Role, Content: m.Content})
		require.NoError(t, err)
	}
	require.NoError(t, e.worker.Work(ctx, &river.Job[MemoryConsolidationArgs]{Args: MemoryConsolidationArgs{UserID: e.user.ID}}))
	require.Len(t, e.llm.requests, 1)
	system := e.llm.requests[0].Messages[0].Content
	assert.Contains(t, system, "вече са известни")
	assert.Contains(t, system, "• Потребителят живее в Бремен.")
}

// A window the model could not answer is not lost: its messages stay unread with an attempt
// counted, and the next run reads them one by one.
func TestAFailedWindowIsReadMessageByMessageNextTime(t *testing.T) {
	e := newConsolidationEnv(t, `not json`, factJSON("location", "София", "Потребителят живее в София", false))
	ctx := context.Background()
	for i, m := range talk("f", 5, 0) {
		content := m.Content
		if i == 0 {
			content = "Живея в София"
		}
		_, err := e.database.Queries.CreateMessage(ctx, db.CreateMessageParams{UserID: e.user.ID, SessionID: "f", Role: m.Role, Content: content})
		require.NoError(t, err)
	}
	job := &river.Job[MemoryConsolidationArgs]{Args: MemoryConsolidationArgs{UserID: e.user.ID}}
	require.NoError(t, e.worker.Work(ctx, job))
	left, err := e.database.Queries.GetUnanalyzedMessages(ctx, e.user.ID)
	require.NoError(t, err)
	assert.Len(t, left, 10, "nothing of the failed window is marked read")
	assert.Empty(t, e.statuses())

	require.NoError(t, e.worker.Work(ctx, job))
	assert.Equal(t, map[string]string{"Потребителят живее в София": "active"}, e.statuses(), "the second run reads them one by one")
	left, err = e.database.Queries.GetUnanalyzedMessages(ctx, e.user.ID)
	require.NoError(t, err)
	assert.Empty(t, left)
}

func zeroVector() pgvector.Vector { return pgvector.NewVector(make([]float32, config.VectorDimension)) }
