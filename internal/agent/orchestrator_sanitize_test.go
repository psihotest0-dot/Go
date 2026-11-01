package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
)

// sanitizeFixture is an orchestrator on the disposable test database whose model
// answers with the given content, split into the given stream chunks.
type sanitizeFixture struct {
	orchestrator *Orchestrator
	database     *db.Database
	userID       uuid.UUID
}

func newSanitizeFixture(t *testing.T, chunks ...string) *sanitizeFixture {
	t.Helper()
	connString := dbtest.ConnString(t)
	t.Chdir("../..")
	prompts.Init("prompts")

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(connString))
	database, err := db.Connect(ctx, connString)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	script := make([]openai.ChatCompletionStreamResponse, 0, len(chunks))
	for _, c := range chunks {
		script = append(script, openai.ChatCompletionStreamResponse{
			Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: c}}},
		})
	}
	mockLLM := &llm.MockLLM{StreamChunks: script}
	retrievalService := retrieval.NewRetrievalService(database, &llm.MockEmbedder{})

	user, err := database.Queries.CreateUser(ctx)
	require.NoError(t, err)
	// Removes only this test's own user; its rows go with it.
	t.Cleanup(func() {
		dbtest.DeleteUser(t, database.Pool, user.ID)
	})

	return &sanitizeFixture{
		orchestrator: NewOrchestrator(database, retrievalService, mockLLM, &MockToolRegistry{}),
		database:     database,
		userID:       user.ID,
	}
}

func (f *sanitizeFixture) request(t *testing.T, path string, body any) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, f.userID))
	return httptest.NewRecorder(), req
}

// storedContents returns the contents of the session's messages for a role,
// newest first.
func (f *sanitizeFixture) storedContents(t *testing.T, sessionID, role string) []string {
	t.Helper()
	msgs, err := f.database.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{
		UserID: f.userID, SessionID: sessionID, Limit: 50,
	})
	require.NoError(t, err)
	var out []string
	for _, m := range msgs {
		if m.Role == role {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestStreamChatStoresAnswerWithoutThinking(t *testing.T) {
	f := newSanitizeFixture(t, "<think>скрити ", "мисли</think>\n\nВидим ", "отговор")

	rec, req := f.request(t, "/chat/stream", ChatRequest{SessionID: "s-think", Content: "Здравей"})
	f.orchestrator.HandleStreamChat(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "скрити")
	require.Equal(t, []string{"Видим отговор"}, f.storedContents(t, "s-think", "assistant"))
}

// Telegram goes through ProcessChat: the answer it returns must already be clean.
func TestProcessChatReturnsAndStoresAnswerWithoutThinking(t *testing.T) {
	f := newSanitizeFixture(t, "<think>скрити мисли</think>", "Видим отговор", "<tool_call></tool_call>")

	answer, err := f.orchestrator.ProcessChat(context.Background(), f.userID, ChatRequest{SessionID: "s-pc", Content: "Здравей"})

	require.NoError(t, err)
	require.Equal(t, "Видим отговор", answer)
	require.Equal(t, []string{"Видим отговор"}, f.storedContents(t, "s-pc", "assistant"))
}

func TestRegenerateStoresAnswerWithoutThinking(t *testing.T) {
	f := newSanitizeFixture(t, "<think>нови мисли</think>Нов отговор")
	ctx := context.Background()
	for _, m := range []struct{ role, content string }{{"user", "Въпрос"}, {"assistant", "Стар отговор"}} {
		_, err := f.database.Queries.CreateMessage(ctx, db.CreateMessageParams{
			UserID: f.userID, SessionID: "s-regen", Role: m.role, Content: m.content,
		})
		require.NoError(t, err)
	}

	rec, req := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-regen"})
	f.orchestrator.HandleRegenerate(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"Нов отговор"}, f.storedContents(t, "s-regen", "assistant"))
}

// A reply that is only thinking must not wipe the answer it was meant to replace.
func TestRegenerateKeepsOldAnswerWhenNewOneIsOnlyThinking(t *testing.T) {
	f := newSanitizeFixture(t, "<think>само мисли, без отговор</think>")
	ctx := context.Background()
	for _, m := range []struct{ role, content string }{{"user", "Въпрос"}, {"assistant", "Стар отговор"}} {
		_, err := f.database.Queries.CreateMessage(ctx, db.CreateMessageParams{
			UserID: f.userID, SessionID: "s-keep", Role: m.role, Content: m.content,
		})
		require.NoError(t, err)
	}

	rec, req := f.request(t, "/chat/regenerate", RegenerateRequest{SessionID: "s-keep"})
	f.orchestrator.HandleRegenerate(rec, req)

	require.Equal(t, []string{"Стар отговор"}, f.storedContents(t, "s-keep", "assistant"))
}
