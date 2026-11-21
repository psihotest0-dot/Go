package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// The cursor of GET /messages: "before" is the oldest message the client shows, the page is the
// ones older than it (plan item (4), step 2).

// storeAt stores a message with the given time and id, as a burst of messages stored in one
// instant is stored; the API cannot make two messages with one time.
func storeAt(t *testing.T, database *db.Database, userID uuid.UUID, session, content string, at time.Time, id uuid.UUID) uuid.UUID {
	t.Helper()
	_, err := database.Pool.Exec(context.Background(),
		`INSERT INTO messages (id, user_id, session_id, role, content, created_at) VALUES ($1, $2, $3, 'user', $4, $5)`,
		id, userID, session, content, at)
	require.NoError(t, err)
	return id
}

// idAscending is the id for the n-th message: the order of the ids is the order of n.
func idAscending(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", n))
}

func getStatus(t *testing.T, apiService *api.API, userID uuid.UUID, rawQuery string) (int, string) {
	t.Helper()
	req := withUser(httptest.NewRequest(http.MethodGet, "/messages"+rawQuery, nil), userID)
	w := httptest.NewRecorder()
	apiService.GetMessagesHandler(w, req)
	return w.Code, w.Body.String()
}

func contents(msgs []api.MessageDTO) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

// Messages stored in one instant are told apart by their ids: paging back through them neither
// repeats nor skips one, whatever the page size is.
func TestMessagesBeforePagesThroughMessagesOfTheSameInstant(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	same := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	older := same.Add(-time.Hour)
	newer := same.Add(time.Hour)

	storeAt(t, database, userID, "page-s", "m0 older", older, idAscending(1))
	for i := 0; i < 5; i++ {
		storeAt(t, database, userID, "page-s", fmt.Sprintf("m%d same", i+1), same, idAscending(10+i))
	}
	storeAt(t, database, userID, "page-s", "m6 newer", newer, idAscending(99))
	want := []string{"m0 older", "m1 same", "m2 same", "m3 same", "m4 same", "m5 same", "m6 newer"}

	for _, size := range []int{1, 2, 3, 6} {
		t.Run(fmt.Sprintf("page of %d", size), func(t *testing.T) {
			// The newest page, then each older one by the oldest message already seen.
			page := getTestMessages(t, apiService, userID, fmt.Sprintf("?session_id=page-s&limit=%d", size))
			require.Len(t, page, size)
			seen := contents(page)
			oldest := page[0].ID
			for len(seen) < len(want)+5 { // the guard against a loop that never ends
				page = getTestMessages(t, apiService, userID, fmt.Sprintf("?session_id=page-s&limit=%d&before=%s", size, oldest))
				if len(page) == 0 {
					break
				}
				seen = append(contents(page), seen...)
				oldest = page[0].ID
			}
			assert.Equal(t, want, seen, "every message once, oldest first")
		})
	}
}

func TestMessagesBeforeReturnsOnlyOlderOnesOldestFirst(t *testing.T) {
	apiService, _, userID := setupAPI(t)
	first := createTestMessage(t, apiService, userID, "older-only", "user", "first")
	second := createTestMessage(t, apiService, userID, "older-only", "assistant", "second")
	third := createTestMessage(t, apiService, userID, "older-only", "user", "third")

	page := getTestMessages(t, apiService, userID, "?session_id=older-only&before="+third.ID.String())
	assert.Equal(t, []string{"first", "second"}, contents(page), "the cursor message itself is not in its page")
	assertOldestFirst(t, page)

	page = getTestMessages(t, apiService, userID, "?session_id=older-only&before="+second.ID.String())
	assert.Equal(t, []string{"first"}, contents(page))

	page = getTestMessages(t, apiService, userID, "?session_id=older-only&before="+first.ID.String())
	assert.Empty(t, page, "nothing is older than the oldest message")
}

func TestMessagesBeforeRespectsTheLimit(t *testing.T) {
	apiService, _, userID := setupAPI(t)
	var last api.MessageDTO
	for i := 0; i < 5; i++ {
		last = createTestMessage(t, apiService, userID, "lim", "user", fmt.Sprintf("m%d", i))
	}

	page := getTestMessages(t, apiService, userID, "?session_id=lim&limit=2&before="+last.ID.String())
	assert.Equal(t, []string{"m2", "m3"}, contents(page), "the two just before the cursor")
}

func TestMessagesBeforeIsRefusedOrNotFoundWhenItMakesNoSense(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	mine := createTestMessage(t, apiService, userID, "mine", "user", "mine")
	createTestMessage(t, apiService, userID, "mine", "user", "mine too")

	other, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })
	theirs := createTestMessage(t, apiService, other.ID, "mine", "user", "not mine")

	tests := []struct {
		name  string
		query string
		code  int
	}{
		{"without a session", "?before=" + mine.ID.String(), http.StatusBadRequest},
		{"not an id", "?session_id=mine&before=yesterday", http.StatusBadRequest},
		{"an id nobody has", "?session_id=mine&before=" + uuid.NewString(), http.StatusNotFound},
		{"another person's message", "?session_id=mine&before=" + theirs.ID.String(), http.StatusNotFound},
		{"my message in another session", "?session_id=elsewhere&before=" + mine.ID.String(), http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getStatus(t, apiService, userID, tc.query)
			assert.Equal(t, tc.code, code, body)
		})
	}
}

// A conversation of the same person with older messages is not part of the page.
func TestMessagesBeforeStaysInsideTheConversation(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	storeAt(t, database, userID, "other-chat", "another chat, older", base.Add(-time.Hour), idAscending(1))
	storeAt(t, database, userID, "this-chat", "this chat, older", base.Add(-30*time.Minute), idAscending(2))
	cursor := storeAt(t, database, userID, "this-chat", "this chat, newest", base, idAscending(3))

	page := getTestMessages(t, apiService, userID, "?session_id=this-chat&before="+cursor.String())

	assert.Equal(t, []string{"this chat, older"}, contents(page))
}

// The page never shows a message of another person, even in a conversation with the same name.
func TestMessagesBeforeNeverShowsAnotherPersonsMessages(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	other, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })

	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	storeAt(t, database, other.ID, "shared-name", "theirs, older", base.Add(-time.Hour), idAscending(1))
	storeAt(t, database, userID, "shared-name", "mine, older", base.Add(-30*time.Minute), idAscending(2))
	cursor := storeAt(t, database, userID, "shared-name", "mine, newest", base, idAscending(3))

	page := getTestMessages(t, apiService, userID, "?session_id=shared-name&before="+cursor.String())
	assert.Equal(t, []string{"mine, older"}, contents(page))
}
