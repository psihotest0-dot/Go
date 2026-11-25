package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/reminders"
)

// textRecorder is a notifier that writes down what it was given.
type textRecorder struct {
	mu  sync.Mutex
	got []string
}

func (r *textRecorder) notify(ctx context.Context, _ uuid.UUID, session, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, session+": "+text)
}

func (r *textRecorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func TestATextOfTheServerGoesOutsideOnlyForASharedConversation(t *testing.T) {
	o := &Orchestrator{}
	var got []string
	user := uuid.New()
	o.SetOutsideNotifier(func(ctx context.Context, userID uuid.UUID, session, text string) {
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline, "a slow Telegram cannot hold the runner")
		assert.Equal(t, user, userID)
		got = append(got, session+": "+text)
	})
	o.NotifyOutside(context.Background(), user, "tg-42", "Готово.")
	o.NotifyOutside(context.Background(), user, "a-chat-of-the-app", "Готово.")
	o.NotifyOutside(context.Background(), user, "tg-42", "")
	assert.Equal(t, []string{"tg-42: Готово."}, got)

	o.SetOutsideNotifier(func(context.Context, uuid.UUID, string, string) { panic("boom") })
	assert.NotPanics(t, func() { o.NotifyOutside(context.Background(), user, "tg-42", "Готово.") })
}

// The code review of 2026-11-25: the sender of the reminders makes its first pass before the bot has
// connected; the reminder was marked sent and never reached Telegram.
func TestATextThatCameBeforeTheBotGoesOutWhenItConnects(t *testing.T) {
	o := &Orchestrator{}
	user := uuid.New()
	o.NotifyOutside(context.Background(), user, "tg-42", "Напомням ти: да платя тока")
	o.NotifyOutside(context.Background(), user, "a-chat-of-the-app", "само в приложението")
	o.NotifyOutside(context.Background(), user, "tg-42", "📊 Сметките за октомври")

	r := &textRecorder{}
	o.SetOutsideNotifier(r.notify)
	o.pendingOutside.wg.Wait()
	assert.Equal(t, []string{"tg-42: Напомням ти: да платя тока", "tg-42: 📊 Сметките за октомври"}, r.texts(), "in their order, once")

	o.NotifyOutside(context.Background(), user, "tg-42", "следващото")
	assert.Len(t, r.texts(), 3, "with the bot there a text goes at once")

	// The bot stops (and connects again): the texts of the time between wait for it again.
	o.SetOutsideNotifier(nil)
	o.NotifyOutside(context.Background(), user, "tg-42", "докато го няма")
	assert.Len(t, r.texts(), 3)
	o.SetOutsideNotifier(r.notify)
	o.pendingOutside.wg.Wait()
	assert.Equal(t, "tg-42: докато го няма", r.texts()[3])
}

func TestTheTextsThatWaitAreBoundedAndGetOld(t *testing.T) {
	o := &Orchestrator{}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	o.pendingOutside.now = func() time.Time { return now }
	user := uuid.New()
	for i := range maxPendingOutside + 1 {
		o.NotifyOutside(context.Background(), user, "tg-42", fmt.Sprintf("текст %d", i))
	}
	r := &textRecorder{}
	o.SetOutsideNotifier(r.notify)
	o.pendingOutside.wg.Wait()
	got := r.texts()
	require.Len(t, got, maxPendingOutside)
	assert.Equal(t, "tg-42: текст 1", got[0], "the oldest went when too many waited")

	o.SetOutsideNotifier(nil)
	o.NotifyOutside(context.Background(), user, "tg-42", "от вчера")
	now = now.Add(pendingOutsideFor + time.Minute)
	o.SetOutsideNotifier(r.notify)
	o.pendingOutside.wg.Wait()
	assert.Len(t, r.texts(), maxPendingOutside, "a text that waited more than a day is old news")
}

// The real road: the sender of the reminders sends one before the bot is there.
func TestAReminderSentBeforeTheBotReachesTelegram(t *testing.T) {
	e := newReminderEnv(t)
	ctx := context.Background()
	_, err := e.store.CreateAt(ctx, e.userID, "tg-42", "да платя тока", time.Now().Add(-time.Minute), reminders.Location())
	require.NoError(t, err)
	runner := reminders.NewRunner(e.db, e.store, e.o.NotifyOutside)
	n, err := runner.Pass(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the reminder is written into its conversation and marked sent")

	r := &textRecorder{}
	e.o.SetOutsideNotifier(r.notify)
	e.o.pendingOutside.wg.Wait()
	got := r.texts()
	require.Len(t, got, 1, "it reaches Telegram once the bot is there")
	assert.Contains(t, got[0], "tg-42: ")
	assert.Contains(t, got[0], "да платя тока")
}

// A text held just as the bot sets itself (NotifyOutside saw nobody, then the bot was set) is sent
// at once, not left in the queue nobody reads any more.
func TestATextHeldAsTheBotSetsItselfIsNotLeftBehind(t *testing.T) {
	o := &Orchestrator{}
	r := &textRecorder{}
	o.SetOutsideNotifier(r.notify)
	n, ok := o.holdOutside(uuid.New(), "tg-42", "точно сега")
	require.True(t, ok)
	require.NotNil(t, n)
	assert.Empty(t, o.pendingOutside.texts)
}
