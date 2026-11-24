package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestATextOfTheServerGoesOutsideOnlyForASharedConversation(t *testing.T) {
	o := &Orchestrator{}
	var got []string
	user := uuid.New()
	o.NotifyOutside(context.Background(), user, "tg-42", "без никого") // nobody set: nothing, no panic

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

	o.SetOutsideNotifier(nil)
	o.NotifyOutside(context.Background(), user, "tg-42", "Готово.")
	assert.Len(t, got, 1)
}
