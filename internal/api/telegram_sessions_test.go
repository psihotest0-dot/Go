package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// GET /telegram/sessions: the Telegram chats tied to the person, as conversations the app can open.

func linkChat(t *testing.T, database *db.Database, chatID int64, userID uuid.UUID) {
	t.Helper()
	require.NoError(t, database.Queries.LinkTelegramUser(context.Background(), db.LinkTelegramUserParams{
		TelegramChatID: chatID, UserID: userID,
	}))
}

func telegramSessions(t *testing.T, apiService *api.API, userID uuid.UUID) (int, api.TelegramSessionsResponse, string) {
	t.Helper()
	mux := http.NewServeMux()
	apiService.RegisterSessionRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/telegram/sessions", nil)
	if userID != uuid.Nil {
		req = withUser(req, userID)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var out api.TelegramSessionsResponse
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	}
	return w.Code, out, w.Body.String()
}

func TestTelegramSessionIDIsWhatTheBotWritesTo(t *testing.T) {
	assert.Equal(t, "tg-1607252727", api.TelegramSessionID(1607252727))
	assert.Equal(t, "tg--1001234567890", api.TelegramSessionID(-1001234567890), "a group has a negative chat id")
}

func TestTelegramSessionsAreEmptyWithoutALink(t *testing.T) {
	apiService, _, userID := setupAPI(t)

	code, out, body := telegramSessions(t, apiService, userID)

	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, out.Sessions)
	assert.JSONEq(t, `{"sessions":[]}`, body, "an empty list, not null")
}

func TestTelegramSessionsListTheLinkedChatsInOrder(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	// Linked in the reverse order: the order of the list is the order of the chat ids.
	linkChat(t, database, 9000000000002, userID)
	linkChat(t, database, 9000000000001, userID)

	code, out, body := telegramSessions(t, apiService, userID)

	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []api.TelegramSessionDTO{
		{SessionID: "tg-9000000000001", Name: "Telegram"},
		{SessionID: "tg-9000000000002", Name: "Telegram 2"},
	}, out.Sessions)
}

func TestTelegramSessionsDoNotShowAnotherPersonsChats(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	other, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })
	linkChat(t, database, 9000000000011, userID)
	linkChat(t, database, 9000000000012, other.ID)

	_, mine, _ := telegramSessions(t, apiService, userID)
	_, theirs, _ := telegramSessions(t, apiService, other.ID)

	assert.Equal(t, []api.TelegramSessionDTO{{SessionID: "tg-9000000000011", Name: "Telegram"}}, mine.Sessions)
	assert.Equal(t, []api.TelegramSessionDTO{{SessionID: "tg-9000000000012", Name: "Telegram"}}, theirs.Sessions)
}

func TestTelegramSessionsNeedAUserAndOnlyAnswerGet(t *testing.T) {
	apiService, _, userID := setupAPI(t)

	code, _, _ := telegramSessions(t, apiService, uuid.Nil)
	assert.Equal(t, http.StatusUnauthorized, code)

	mux := http.NewServeMux()
	apiService.RegisterSessionRoutes(mux)
	req := withUser(httptest.NewRequest(http.MethodPost, "/telegram/sessions", nil), userID)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "the list is read only")
}
