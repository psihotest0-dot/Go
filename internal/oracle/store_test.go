package oracle

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

func openStore(t *testing.T) (*DBStore, *db.Database, string) {
	t.Helper()
	dbURL := dbtest.ConnString(t)
	t.Chdir("../..") // the migrations are found relative to the working directory
	// The throwaway database is brought to the newest schema here, so the test does not
	// depend on which package ran before it.
	require.NoError(t, db.RunMigrations(dbURL))

	ctx := context.Background()
	database, err := db.Connect(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	// Every row of this test has a signal id that starts with the prefix, and only
	// those are removed.
	prefix := "test-" + uuid.NewString()[:8] + "-"
	t.Cleanup(func() {
		_, err := database.SqlDB.ExecContext(context.Background(),
			`DELETE FROM oracle_evaluations WHERE signal_id LIKE $1`, prefix+"%")
		assert.NoError(t, err)
	})
	return NewDBStore(database.Queries), database, prefix
}

func answered(prefix, id, symbol, verdict string) Evaluation {
	return Evaluation{
		RequestID: "req-" + id, SignalID: prefix + id, Symbol: symbol, Mode: ModeShadow,
		Status: StatusAnswered, Verdict: verdict, Reason: "Причина " + id, Model: "gemma",
		Attempts: 1, FinishReason: "stop", PromptTokens: 700, CompletionTokens: 40, LatencyMS: 3500,
		Request: []byte(`{"schema_version":1,"signal":{"symbol":"` + symbol + `"}}`),
	}
}

func TestDBStoreSavesAndListsTheJournal(t *testing.T) {
	store, _, prefix := openStore(t)
	ctx := context.Background()

	saved, err := store.Save(ctx, answered(prefix, "a", "AAPL", VerdictHold))
	require.NoError(t, err)
	assert.NotZero(t, saved.ID)
	assert.False(t, saved.CreatedAt.IsZero())

	failed := Evaluation{
		RequestID: "req-b", SignalID: prefix + "b", Symbol: "AAPL", Status: StatusFailed,
		Attempts: 2, FinishReason: "length", Error: "the model's reply cannot be used", LatencyMS: 9000,
		Request: []byte(`{}`),
	}
	_, err = store.Save(ctx, failed)
	require.NoError(t, err)
	_, err = store.Save(ctx, answered(prefix, "c", "MSFT", VerdictVeto))
	require.NoError(t, err)

	rows, err := store.List(ctx, ListFilter{Limit: 200})
	require.NoError(t, err)
	var mine []Evaluation
	for _, r := range rows {
		if len(r.SignalID) > len(prefix) && r.SignalID[:len(prefix)] == prefix {
			mine = append(mine, r)
		}
	}
	require.Len(t, mine, 3)
	assert.Equal(t, prefix+"c", mine[0].SignalID, "the newest first")
	assert.Equal(t, prefix+"a", mine[2].SignalID)

	assert.Equal(t, VerdictVeto, mine[0].Verdict)
	assert.Equal(t, "Причина c", mine[0].Reason)
	assert.Equal(t, "gemma", mine[0].Model)
	assert.Equal(t, ModeShadow, mine[0].Mode)
	assert.Equal(t, 3500, mine[0].LatencyMS)

	assert.Equal(t, StatusFailed, mine[1].Status)
	assert.Empty(t, mine[1].Verdict, "a failure has no verdict")
	assert.Equal(t, "length", mine[1].FinishReason)
	assert.Equal(t, 2, mine[1].Attempts)
	assert.Nil(t, mine[0].Request, "the list leaves the request bodies out")
}

func TestDBStoreKeepsTheRequestBody(t *testing.T) {
	store, database, prefix := openStore(t)

	e := answered(prefix, "body", "AAPL", VerdictApprove)
	e.Request = []byte(`{"schema_version":1,"news":[{"headline":"Привет"}]}`)
	_, err := store.Save(context.Background(), e)
	require.NoError(t, err)

	var got string
	require.NoError(t, database.SqlDB.QueryRowContext(context.Background(),
		`SELECT request::text FROM oracle_evaluations WHERE signal_id = $1`, prefix+"body").Scan(&got))
	assert.JSONEq(t, `{"schema_version":1,"news":[{"headline":"Привет"}]}`, got)

	// No body at all is stored as an empty object, not refused.
	e = answered(prefix, "nobody", "AAPL", VerdictApprove)
	e.Request = nil
	_, err = store.Save(context.Background(), e)
	require.NoError(t, err)
}

func TestDBStorePagesAndFilters(t *testing.T) {
	store, _, prefix := openStore(t)
	ctx := context.Background()
	var ids []int64
	for i, sym := range []string{"AAPL", "MSFT", "AAPL", "MSFT", "AAPL"} {
		saved, err := store.Save(ctx, answered(prefix, string(rune('a'+i)), sym, VerdictHold))
		require.NoError(t, err)
		ids = append(ids, saved.ID)
	}

	page1, err := store.List(ctx, ListFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1, 2)
	assert.Equal(t, ids[4], page1[0].ID)

	page2, err := store.List(ctx, ListFilter{Limit: 2, BeforeID: page1[1].ID})
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.Equal(t, ids[2], page2[0].ID, "the next page continues where the first ended")

	msft, err := store.List(ctx, ListFilter{Symbol: "MSFT", Limit: 200})
	require.NoError(t, err)
	for _, r := range msft {
		assert.Equal(t, "MSFT", r.Symbol)
	}
}

// The rules of the journal are in the schema, not only in the code that writes it.
func TestTheSchemaRefusesAnInconsistentRow(t *testing.T) {
	_, database, prefix := openStore(t)
	ctx := context.Background()

	insert := func(signalID, symbol, mode, status string, verdict any, latency int) error {
		_, err := database.SqlDB.ExecContext(ctx, `
			INSERT INTO oracle_evaluations (signal_id, symbol, mode, status, verdict, latency_ms, request)
			VALUES ($1, $2, $3, $4, $5, $6, '{}')`, signalID, symbol, mode, status, verdict, latency)
		return err
	}

	require.NoError(t, insert(prefix+"ok1", "AAPL", "shadow", "answered", "HOLD", 10), "a consistent row is accepted")
	require.NoError(t, insert(prefix+"ok2", "AAPL", "", "failed", nil, 10))

	tests := []struct {
		name string
		err  error
	}{
		{"a verdict that is not one of the three", insert(prefix+"x1", "AAPL", "", "answered", "MAYBE", 10)},
		{"an answer without a verdict", insert(prefix+"x2", "AAPL", "", "answered", nil, 10)},
		{"a failure with a verdict", insert(prefix+"x3", "AAPL", "", "failed", "APPROVE", 10)},
		{"an unknown status", insert(prefix+"x4", "AAPL", "", "pending", nil, 10)},
		{"an empty signal id", insert("", "AAPL", "", "failed", nil, 10)},
		{"an empty symbol", insert(prefix+"x5", "", "", "failed", nil, 10)},
		{"an unknown mode", insert(prefix+"x6", "AAPL", "yolo", "failed", nil, 10)},
		{"a negative latency", insert(prefix+"x7", "AAPL", "", "failed", nil, -1)},
	}
	for _, tt := range tests {
		assert.Error(t, tt.err, tt.name)
	}
}

// The journal tells the bot's test questions from its signals by the version the bot
// sent, and the periods of the prompt by the label the Service stamped.
func TestDBStoreKeepsThePromptVersionAndReadsTheSignalVersion(t *testing.T) {
	store, _, prefix := openStore(t)
	ctx := context.Background()

	probe := answered(prefix, "probe", "AAPL", VerdictVeto)
	probe.PromptVersion = "0123456789ab"
	probe.Request = []byte(`{"schema_version":1,"signal":{"symbol":"AAPL","version":"oracle-probe"}}`)
	_, err := store.Save(ctx, probe)
	require.NoError(t, err)

	real := answered(prefix, "real", "KO", VerdictApprove)
	real.PromptVersion = "fedcba987654"
	real.Request = []byte(`{"schema_version":1,"signal":{"symbol":"KO","version":"1.0.0"}}`)
	_, err = store.Save(ctx, real)
	require.NoError(t, err)

	old := answered(prefix, "old", "MSFT", VerdictHold) // no label, and a body that names no version
	old.Request = []byte(`{"schema_version":1}`)
	_, err = store.Save(ctx, old)
	require.NoError(t, err)

	rows, err := store.List(ctx, ListFilter{Limit: 200})
	require.NoError(t, err)
	got := map[string]Evaluation{}
	for _, r := range rows {
		got[r.SignalID] = r
	}

	assert.Equal(t, "oracle-probe", got[prefix+"probe"].SignalVersion)
	assert.Equal(t, "0123456789ab", got[prefix+"probe"].PromptVersion)
	assert.Equal(t, "1.0.0", got[prefix+"real"].SignalVersion)
	assert.Equal(t, "fedcba987654", got[prefix+"real"].PromptVersion)
	assert.Empty(t, got[prefix+"old"].SignalVersion, "a body without a version has none")
	assert.Empty(t, got[prefix+"old"].PromptVersion, "not recorded")
}

func TestTheSchemaGuardsTheTwoVersionColumns(t *testing.T) {
	_, database, prefix := openStore(t)
	ctx := context.Background()

	insert := func(signalID, promptVersion string) error {
		_, err := database.SqlDB.ExecContext(ctx, `
			INSERT INTO oracle_evaluations (signal_id, symbol, status, prompt_version, request)
			VALUES ($1, 'AAPL', 'failed', $2, '{}')`, signalID, promptVersion)
		return err
	}
	require.NoError(t, insert(prefix+"v-empty", ""), "not recorded is allowed")
	require.NoError(t, insert(prefix+"v-hex", "0123456789ab"))

	assert.Error(t, insert(prefix+"v-short", "0123456789a"), "11 characters")
	assert.Error(t, insert(prefix+"v-long", "0123456789abc"), "13 characters")
	assert.Error(t, insert(prefix+"v-upper", "0123456789AB"), "upper case")
	assert.Error(t, insert(prefix+"v-text", "not-a-hash!!"), "not hex")

	// signal_version is generated from the request: it cannot be written, so it cannot
	// disagree with the body it is read from.
	_, err := database.SqlDB.ExecContext(ctx, `
		INSERT INTO oracle_evaluations (signal_id, symbol, status, signal_version, request)
		VALUES ($1, 'AAPL', 'failed', 'forged', '{"signal":{"version":"1.0.0"}}')`, prefix+"v-forged")
	assert.Error(t, err, "a generated column refuses a value")
}
