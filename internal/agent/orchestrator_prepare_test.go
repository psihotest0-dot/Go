package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

func storeTurnMessages(t *testing.T, e *attachmentEnv, session string, messages ...[2]string) {
	t.Helper()
	for _, m := range messages {
		_, err := e.db.Queries.CreateMessage(context.Background(), db.CreateMessageParams{
			UserID: e.userID, SessionID: session, Role: m[0], Content: m[1],
			SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true},
		})
		require.NoError(t, err)
	}
}

func contents(messages []openai.ChatCompletionMessage) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, m.Role+": "+m.Content)
	}
	return out
}

func traceSteps(t *testing.T, e *attachmentEnv, session string) []string {
	t.Helper()
	rows, err := e.db.Pool.Query(context.Background(),
		"SELECT step FROM agent_traces WHERE user_id=$1 AND session_id=$2 ORDER BY created_at, id", e.userID, session)
	require.NoError(t, err)
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		steps = append(steps, s)
	}
	require.NoError(t, rows.Err())
	return steps
}

// prepareTurn is the one place that builds the prompt of a turn for the chat stream,
// regenerate and Telegram (they used to carry three copies).
func TestPrepareTurnBuildsTheModelRequest(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	storeTurnMessages(t, e, "s1",
		[2]string{"user", "първи въпрос"}, [2]string{"assistant", "първи отговор"},
		[2]string{"user", "втори въпрос"})
	e.attach(t, e.userID, "s1", "notes.go", "package notes\n")

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "s1", Content: "втори въпрос"}, false)
	require.NoError(t, err)

	require.Len(t, prep.messages, 4, "the system prompt and the three stored messages")
	assert.Equal(t, openai.ChatMessageRoleSystem, prep.messages[0].Role)
	assert.Contains(t, prep.messages[0].Content, "notes.go", "the attached file is part of the prompt")
	assert.Equal(t, []string{"system", "user: първи въпрос", "assistant: първи отговор", "user: втори въпрос"},
		append([]string{"system"}, contents(prep.messages[1:])...))
	assert.NotEmpty(t, prep.tools, "the tool definitions are part of the request")
	assert.Same(t, prep.turn, turnFrom(prep.ctx), "the state of the turn travels in the context the model call runs with")
	assert.Equal(t, []string{sourceAttachments}, prep.turn.externalSources(), "text read from an attached file is marked as untrusted for the proposals of the turn")
	assert.Equal(t, []string{"search", "prune"}, traceSteps(t, e, "s1"), "both trace steps are written, in order")
}

// Regenerating writes a new reply: the one being replaced must not be in the prompt, or the
// model would read and copy it.
func TestPrepareTurnForRegenerationLeavesTheOldAnswerOut(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	storeTurnMessages(t, e, "s2",
		[2]string{"user", "въпрос"}, [2]string{"assistant", "стар отговор"})
	req := ChatRequest{SessionID: "s2", Content: "въпрос"}

	fresh, err := o.prepareTurn(context.Background(), e.userID, req, false)
	require.NoError(t, err)
	regenerated, err := o.prepareTurn(context.Background(), e.userID, req, true)
	require.NoError(t, err)

	assert.Equal(t, []string{"user: въпрос", "assistant: стар отговор"}, contents(fresh.messages[1:]))
	assert.Equal(t, []string{"user: въпрос"}, contents(regenerated.messages[1:]), "only the old answer is dropped")
}

func TestPrepareTurnReportsAFailureWithoutAModelCall(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the database refuses a query on a cancelled context

	_, err := o.prepareTurn(ctx, e.userID, ChatRequest{SessionID: "s3", Content: "въпрос"}, false)

	require.Error(t, err)
}

// The same through the HTTP handler: the model is asked to write a new reply and must not be
// shown the one it replaces, and the new reply takes the place of the old one.
func TestRegenerateDoesNotShowTheModelTheAnswerItReplaces(t *testing.T) {
	e := newAttachmentEnv(t)
	model := answeringModel()
	o := orchestratorWith(t, e, model)
	storeTurnMessages(t, e, "s4",
		[2]string{"user", "кажи нещо"}, [2]string{"assistant", "СТАР-ОТГОВОР-ЗА-ЗАМЯНА"})

	body, _ := json.Marshal(RegenerateRequest{SessionID: "s4"})
	req := httptest.NewRequest(http.MethodPost, "/chat/regenerate", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleRegenerate(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.NotEmpty(t, model.Requests)
	for _, m := range model.Requests[0].Messages {
		assert.NotContains(t, m.Content, "СТАР-ОТГОВОР-ЗА-ЗАМЯНА", "the model must write the reply, not copy the old one")
	}
	stored, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "s4", Limit: 10})
	require.NoError(t, err)
	var assistant []string
	for _, m := range stored {
		if m.Role == "assistant" {
			assistant = append(assistant, m.Content)
		}
	}
	assert.Equal(t, []string{"Видях го."}, assistant, "exactly one answer remains: the new one")
}
