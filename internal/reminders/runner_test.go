package reminders

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

type sentTo struct {
	session string
	text    string
}

type recorder struct {
	mu   sync.Mutex
	sent []sentTo
}

func (r *recorder) notify(_ context.Context, _ uuid.UUID, session, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sentTo{session, text})
}

func newRunnerEnv(t *testing.T) (*db.Database, *Store, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Connect(ctx, dbtest.ConnString(t))
	require.NoError(t, err)
	t.Cleanup(database.Close)
	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, user.ID) })
	return database, NewStore(database), user.ID
}

// keep stores a reminder the way the chat does: proposed, then kept.
func keep(t *testing.T, s *Store, userID uuid.UUID, session string, d Draft, now time.Time) Reminder {
	t.Helper()
	p, err := s.Propose(context.Background(), userID, session, d)
	require.NoError(t, err)
	kept, ok, err := s.Activate(context.Background(), userID, p, now, berlin)
	require.NoError(t, err)
	require.True(t, ok)
	return kept
}

func TestTheSenderWritesTheReminderOnceAndSendsItToTelegram(t *testing.T) {
	database, store, userID := newRunnerEnv(t)
	ctx := context.Background()
	_, err := database.SqlDB.Exec(`INSERT INTO telegram_users (telegram_chat_id, user_id) VALUES ($1, $2)`, 4242000+time.Now().UnixNano()%1000, userID)
	require.NoError(t, err)
	chat, ok, err := store.TelegramChat(ctx, userID)
	require.NoError(t, err)
	require.True(t, ok)

	made := at(2026, 11, 24, 20, 41)
	once := keep(t, store, userID, "app-chat", Draft{Text: "да платя тока", Rule: Rule{First: Day{2026, 11, 25}, Minute: 9 * 60}}, made)
	daily := keep(t, store, userID, "app-chat", Draft{Text: "да си пия хапчето", Rule: Rule{First: Day{2026, 11, 25}, Minute: 8 * 60, Repeat: Daily}}, made)
	require.Equal(t, at(2026, 11, 25, 9, 0), once.Next.In(berlin))

	rec := &recorder{}
	r := NewRunner(database, store, rec.notify)
	// The computer was off in the morning: it is 11:30.
	r.now = func() time.Time { return at(2026, 11, 25, 11, 30) }
	n, err := r.Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, err = r.Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a reminder is sent once")

	require.Len(t, rec.sent, 2)
	for _, s := range rec.sent {
		assert.Equal(t, "tg-"+strconv.FormatInt(chat, 10), s.session, "to the chat linked to the person")
		assert.Contains(t, s.text, "закъсняло")
	}
	msgs, err := database.Queries.GetMessagesBySession(ctx, db.GetMessagesBySessionParams{UserID: userID, SessionID: "app-chat", Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs, 2, "both are written into the conversation they were made in")

	active, err := store.Active(ctx, userID)
	require.NoError(t, err)
	require.Len(t, active, 1, "the one that is not repeated is over")
	assert.Equal(t, daily.ID, active[0].ID)
	assert.Equal(t, at(2026, 11, 26, 8, 0), active[0].Next.In(berlin), "the daily one moves to tomorrow, the missed ones are not repeated")
}

func TestAProposalIsNotSent(t *testing.T) {
	database, store, userID := newRunnerEnv(t)
	_, err := store.Propose(context.Background(), userID, "app-chat", Draft{Text: "x", Rule: Rule{First: Day{2026, 11, 25}, Minute: 9 * 60}, Next: at(2026, 11, 25, 9, 0)})
	require.NoError(t, err)
	r := NewRunner(database, store, nil)
	r.now = func() time.Time { return at(2026, 11, 26, 9, 0) }
	n, err := r.Pass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "only what the person said yes to is sent")
}

func TestAYesAfterTheTimeKeepsNothing(t *testing.T) {
	_, store, userID := newRunnerEnv(t)
	p, err := store.Propose(context.Background(), userID, "app-chat", Draft{Text: "x", Rule: Rule{First: Day{2026, 11, 25}, Minute: 9 * 60}, Next: at(2026, 11, 25, 9, 0)})
	require.NoError(t, err)
	_, ok, err := store.Activate(context.Background(), userID, p, at(2026, 11, 25, 9, 1), berlin)
	require.NoError(t, err)
	assert.False(t, ok)
	proposed, err := store.Proposed(context.Background(), userID, "app-chat")
	require.NoError(t, err)
	assert.Nil(t, proposed)
}

