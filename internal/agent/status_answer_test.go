package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
)

func TestFilesStatusText(t *testing.T) {
	now := time.Date(2026, 11, 22, 23, 0, 0, 0, time.Local)
	since := now.Add(-time.Hour)

	none := filesStatusText(nil, since, now)
	assert.True(t, strings.HasPrefix(none, "Не. От молбата ти в 22:00 нито един файл не е променян"), none)

	text := filesStatusText([]userfiles.Info{
		{Name: "бележки.md", Size: 2355, Modified: now.Add(-30 * time.Minute)},
		{Name: "стар.txt", Size: 12, Modified: now.Add(-25 * time.Hour)},
	}, since, now)
	assert.Contains(t, text, "от молбата ти в 22:00 насам")
	assert.Contains(t, text, "• бележки.md — в 22:30, 2,3 KB")
	assert.Contains(t, text, "• стар.txt — в 21.11 22:00, 12 байта", "another day shows the date")
	assert.NotContains(t, text, "успях", "the code says what changed, not that the work is done")

	var many []userfiles.Info
	for range statusAnswerMaxFiles + 3 {
		many = append(many, userfiles.Info{Name: "f.md", Modified: now})
	}
	assert.Contains(t, filesStatusText(many, since, now), "• и още 3")
	assert.Equal(t, "1,5 MB", size(3*1024*1024/2))
}

func TestPlanStatusText(t *testing.T) {
	p := &taskplan.Plan{Goal: "21 източника във файлове", Steps: []taskplan.Step{
		{ID: 1, Text: "статия А", Status: taskplan.StepDone},
		{ID: 2, Text: "статия Б", Status: taskplan.StepFailed},
		{ID: 3, Text: "статия В", Status: taskplan.StepSkipped},
		{ID: 4, Text: "статия Г", Status: taskplan.StepPending},
	}}
	text := planStatusText(p)
	assert.Contains(t, text, "Планът „21 източника във файлове“: готови 1 от 4 стъпки, неуспешни 1, пропуснати 1.")
	assert.Contains(t, text, "• не стана: 2. статия Б")
	assert.Contains(t, text, "Следва: 4. статия Г")
}

// listerRegistry lists the files it is given.
type listerRegistry struct {
	fakeToolRegistry
	files []userfiles.Info
}

func (l *listerRegistry) ListUserFiles(uuid.UUID) ([]userfiles.Info, error) { return l.files, nil }

func TestTheCodeAnswersOnlyAShortQuestionAboutWorkOnTheFiles(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	u := func(c string) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: "user", Content: c}
	}
	a := func(c string) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: "assistant", Content: c}
	}
	conversation := func(request, reply, question string) ([]openai.ChatCompletionMessage, []time.Time) {
		return []openai.ChatCompletionMessage{u(request), a(reply), u(question)}, []time.Time{t0, t0, t0.Add(30 * time.Minute)}
	}
	reg := &listerRegistry{files: []userfiles.Info{
		{Name: "статия.md", Size: 100, Modified: t0.Add(10 * time.Minute)},
		{Name: "стар.md", Size: 100, Modified: t0.Add(-time.Hour)},
	}}
	o := (&Orchestrator{tools: reg}).WithFileTools()
	answer := func(o *Orchestrator, request, reply, question string, plan *taskplan.Plan) string {
		h, at := conversation(request, reply, question)
		return o.statusAnswerFor(context.Background(), uuid.New(), question, h, at, plan)
	}
	const request = "запиши цялата статия в статия.md"

	assert.Empty(t, answer(o, request, "Ще започна", "успя ли?", nil), "off by default")
	o.WithStatusAnswer()

	got := answer(o, request, "Ще започна", "успя ли?", nil)
	assert.Contains(t, got, "• статия.md")
	assert.NotContains(t, got, "стар.md", "only the files changed since the request")

	reg.files = reg.files[1:]
	assert.True(t, strings.HasPrefix(answer(o, request, "Ще започна", "готово ли е?", nil), "Не."), "nothing changed: said so")
	assert.True(t, strings.HasPrefix(answer(o, "можеш ли да подредиш нещата по-добре", "Ще обновя файла веднага.", "докъде стигна?", nil), "Не."),
		"the request names no file, the answer after it does")

	for _, c := range []struct{ name, request, question string }{
		{"not about the files", "намери ми книгата на Фром за любовта", "успя ли?"},
		{"not a question about the work", request, "благодаря ти много"},
		{"more than the question", request, "успя ли? ако не, запиши го в бележки.md"},
		{"too long", request, "успя ли " + strings.Repeat("да видиш ", 12)},
	} {
		assert.Empty(t, answer(o, c.request, "Добре.", c.question, nil), c.name)
	}

	plan := &taskplan.Plan{Goal: "цел", Steps: []taskplan.Step{{ID: 1, Text: "a", Status: taskplan.StepDone}}}
	assert.Contains(t, answer(o, "намери ми книгата на Фром за любовта", "Добре.", "докъде си?", plan), "Планът „цел“: готови 1 от 1",
		"a plan in progress answers whatever the work")

	noFiles := (&Orchestrator{tools: reg}).WithStatusAnswer()
	assert.Empty(t, answer(noFiles, request, "Ще започна", "успя ли?", nil), "without the file tools the folder says nothing")
}

func TestACodeAnswerAsksNoModel(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Да, успях!")}}
	o := newGenerateOrchestrator(mock, &fakeToolRegistry{})
	ctx, turn := withTurn(context.Background(), "s", false)
	turn.setStatusAnswer("Не. Нищо не е записано.")
	var streamed strings.Builder
	content, executed, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{{Role: "user", Content: "успя ли?"}}, toolDefinitions(),
		func(c, _ string) error { streamed.WriteString(c); return nil })
	require.NoError(t, err)
	assert.Equal(t, "Не. Нищо не е записано.", content)
	assert.Equal(t, content, streamed.String(), "the client gets it as a stream")
	assert.Empty(t, executed)
	assert.Empty(t, mock.Requests, "the model was not asked")
}

// The whole turn: the request, an answer that promised, the question - the stream carries the code's
// answer, the model is not asked, and the answer is stored.
func TestAStatusQuestionIsAnsweredFromTheFolder(t *testing.T) {
	e := newAttachmentEnv(t)
	dir := filepath.Join(t.TempDir(), "files")
	store, err := userfiles.Open(dir, userfiles.Limits{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	e.reg.WithFiles(store)
	_, err = store.Create(e.userID, "стар.md", "text")
	require.NoError(t, err)
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, e.userID.String(), "стар.md"), old, old))

	model := answeringModel()
	o := orchestratorWith(t, e, model).WithFileTools().WithStatusAnswer()
	storeTurnMessages(t, e, "st-1", [2]string{"user", "запиши цялата статия в статия.md"}, [2]string{"assistant", "Ще започна веднага."})

	body, _ := json.Marshal(ChatRequest{SessionID: "st-1", Content: "успя ли?"})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Contains(t, rec.Body.String(), "нито един файл не е променян")
	assert.Contains(t, rec.Body.String(), "data: [DONE]")
	assert.Empty(t, model.Requests, "the model was not asked")
	stored, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "st-1", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, stored)
	assert.Equal(t, "assistant", stored[0].Role)
	assert.Contains(t, stored[0].Content, "нито един файл не е променян", "the answer is stored")
}
