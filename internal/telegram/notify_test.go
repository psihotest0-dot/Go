package telegram

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The report of a task the server walked in a shared conversation goes to the chat.
func TestTheReportOfATaskGoesToTheLinkedChat(t *testing.T) {
	user := uuid.New()
	f := mirrorFixture(42, user)

	f.bot.Notify(context.Background(), user, "tg-42", "Готово: ключовите факти на всеки от 2 документа — 2 от 2.")

	require.Len(t, f.sender.messages, 1)
	assert.Equal(t, int64(42), f.sender.messages[0].ChatID)
	assert.Contains(t, f.sender.messages[0].Text, "Готово: ключовите факти")
}

func TestTheReportGoesNowhereItDoesNotBelong(t *testing.T) {
	user, stranger := uuid.New(), uuid.New()
	for name, c := range map[string]struct {
		user    uuid.UUID
		session string
		linked  map[int64]uuid.UUID
		allowed []int64
	}{
		"a conversation of the app":      {user, "4b1c0000-0000-4000-8000-000000000001", map[int64]uuid.UUID{42: user}, []int64{42}},
		"a chat linked to another user":  {stranger, "tg-42", map[int64]uuid.UUID{42: user}, []int64{42}},
		"a chat that is not linked":      {user, "tg-42", map[int64]uuid.UUID{}, []int64{42}},
		"a chat the bot may not talk to": {user, "tg-42", map[int64]uuid.UUID{42: user}, []int64{7}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(c.allowed...)
			for k, v := range c.linked {
				f.store.linked[k] = v
			}
			f.bot.Notify(context.Background(), c.user, c.session, "Готово.")
			assert.Empty(t, f.sender.messages)
		})
	}
}
