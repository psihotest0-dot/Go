package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/answerplan"
	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
)

// planEmbedder is the embedder of the planner in these tests: the personal examples on one axis,
// the not-personal ones on another, and the vectors of the test messages given by the test.
// The search of the turn keeps llm.MockEmbedder; a memory is stored with the mock vector of the
// message, so that the search finds it and the planner sees it as fitting.
type planEmbedder struct {
	passages, queries map[string][]float32
}

func axisVector(i int) []float32 {
	v := make([]float32, config.VectorDimension)
	v[i] = 1
	return v
}

func mockVector(t *testing.T, text string) []float32 {
	t.Helper()
	v, err := (&llm.MockEmbedder{}).EmbedTexts(context.Background(), []string{text}, true)
	require.NoError(t, err)
	return v[0]
}

func newPlanEmbedder() *planEmbedder {
	e := &planEmbedder{passages: map[string][]float32{}, queries: map[string][]float32{}}
	personal, not := answerplan.Examples()
	for _, s := range personal {
		e.passages[s] = axisVector(0)
	}
	for _, s := range not {
		e.passages[s] = axisVector(1)
		e.queries[s] = axisVector(1)
	}
	return e
}

func (e *planEmbedder) EmbedTexts(_ context.Context, texts []string, isQuery bool) ([][]float32, error) {
	table := e.passages
	if isQuery {
		table = e.queries
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := table[t]
		if !ok {
			return nil, errors.New("no vector for " + t)
		}
		out[i] = v
	}
	return out, nil
}

const (
	tiredMessage = "уморен съм от нощните смени"
	techMessage  = "обясни ми как работи пакетът sync"
	sofaMemory   = "Ивайло си почива на дивана пред телевизора."
)

// plannedEnv is an orchestrator with the planner on and one memory that fits tiredMessage.
func plannedEnv(t *testing.T, model *llm.MockLLM) (*attachmentEnv, *Orchestrator, uuid.UUID) {
	t.Helper()
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, model)
	pe := newPlanEmbedder()
	pe.passages[tiredMessage] = axisVector(0)
	pe.queries[tiredMessage] = mockVector(t, tiredMessage)
	pe.passages[techMessage] = axisVector(1)
	pe.queries[techMessage] = mockVector(t, techMessage)
	planner, err := answerplan.NewPlanner(context.Background(), pe)
	require.NoError(t, err)
	o.WithAnswerPlanner(planner)

	m, err := e.db.Queries.CreateMemory(context.Background(), db.CreateMemoryParams{
		UserID: e.userID, Content: sofaMemory, Status: "active",
		Embedding:      pgvector.NewVector(mockVector(t, tiredMessage)),
		EmbeddingModel: config.DefaultEmbedModel, Importance: 5,
		ContentHash: uuid.NewString(),
	})
	require.NoError(t, err)
	return e, o, m.ID
}

func TestAPersonalMessageGetsThePlanInPlaceOfTheClosingNote(t *testing.T) {
	e, o, memID := plannedEnv(t, answeringModel())
	storeReplies(t, e, "ap-1", 0, tiredMessage)

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "ap-1", Content: tiredMessage}, false)
	require.NoError(t, err)

	last := lastMessage(t, prep)
	assert.True(t, strings.HasPrefix(last, tiredMessage+"\n\n[Note for the answer"), last)
	assert.Contains(t, last, "[1] "+sofaMemory)
	assert.Contains(t, last, "not with a question", "the first reply ends with a statement, as the dose says")
	assert.NotContains(t, last, closingNoteStatement, "the plan takes the place of the closing note")
	plan := prep.turn.answerPlan()
	require.NotNil(t, plan)
	require.Len(t, plan.Memories, 1)
	assert.Equal(t, memID, plan.Memories[0].ID)
}

func TestAMessageThatIsNotPersonalKeepsTheClosingNote(t *testing.T) {
	e, o, _ := plannedEnv(t, answeringModel())
	storeReplies(t, e, "ap-2", 0, techMessage)

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "ap-2", Content: techMessage}, false)
	require.NoError(t, err)
	assert.Equal(t, techMessage+closingNoteStatement, lastMessage(t, prep))
	assert.Nil(t, prep.turn.answerPlan())
}

func TestWithoutThePlannerNothingChanges(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	storeReplies(t, e, "ap-3", 0, tiredMessage)
	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "ap-3", Content: tiredMessage}, false)
	require.NoError(t, err)
	assert.Equal(t, tiredMessage+closingNoteStatement, lastMessage(t, prep))
	assert.Nil(t, prep.turn.answerPlan())
}

// The whole turn: the model marks the memory it used, the person sees no mark (not even one
// split between two pieces of the stream), the stored answer has no mark and keeps the plan,
// and the next reply of the conversation has no plan (one in five).
func TestAPlannedTurnHidesTheMarksAndStoresThePlan(t *testing.T) {
	model := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{
		textChunk("Почини си на дивана, пред телевизора ["), textChunk("1]. Заслужаваш го [7]."),
	}}
	e, o, memID := plannedEnv(t, model)

	body, _ := json.Marshal(ChatRequest{SessionID: "ap-4", Content: tiredMessage})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	streamed := streamedText(t, rec.Body.String())
	assert.Equal(t, "Почини си на дивана, пред телевизора. Заслужаваш го.", streamed)

	stored, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "ap-4", Limit: 10})
	require.NoError(t, err)
	require.Len(t, stored, 2)
	answer := stored[0]
	require.Equal(t, "assistant", answer.Role)
	assert.Equal(t, streamed, answer.Content)
	require.True(t, answer.AnswerPlan.Valid)
	var p storedPlan
	require.NoError(t, json.Unmarshal(answer.AnswerPlan.RawMessage, &p))
	require.Len(t, p.Memories, 1)
	assert.Equal(t, memID, p.Memories[0].ID)
	assert.Equal(t, sofaMemory, p.Memories[0].Label)
	assert.Equal(t, []int{1}, p.Marks)
	assert.Equal(t, 1, p.UnknownMarks, "[7] names no memory of the plan")
	assert.Equal(t, "thought", p.Mode)
	assert.False(t, stored[1].AnswerPlan.Valid, "the person's message has no plan")

	// The next personal message of the conversation: too soon for a plan.
	storeTurnMessages(t, e, "ap-4", [2]string{"user", tiredMessage})
	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "ap-4", Content: tiredMessage}, false)
	require.NoError(t, err)
	assert.Nil(t, prep.turn.answerPlan())
	assert.Equal(t, tiredMessage+closingNoteStatement, lastMessage(t, prep))
}

// The end of a planned answer that may have been the start of a mark (a space, a "[") is sent
// when the stream ends.
func TestAPlannedStreamSendsItsLastCharacters(t *testing.T) {
	model := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{
		textChunk("Почини си [1] и "), textChunk("виж ["),
	}}
	e, o, _ := plannedEnv(t, model)
	body, _ := json.Marshal(ChatRequest{SessionID: "ap-5", Content: tiredMessage})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "Почини си и виж [", streamedText(t, rec.Body.String()))
}

// Telegram stores and returns the answer without marks too.
func TestATelegramTurnOfAPlanHidesTheMarks(t *testing.T) {
	model := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Почини си [1].")}}
	e, o, _ := plannedEnv(t, model)

	answer, err := o.ProcessChat(context.Background(), e.userID, ChatRequest{SessionID: "tg-ap", Content: tiredMessage})
	require.NoError(t, err)
	assert.Equal(t, "Почини си.", answer)
	stored, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "tg-ap", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, "Почини си.", stored[0].Content)
	assert.True(t, stored[0].AnswerPlan.Valid)
}

// A memory of the person's last plans waits while there are others; with no other, it is used again.
func TestRecentlyPlannedMemoriesComeFromTheStoredPlans(t *testing.T) {
	e, o, memID := plannedEnv(t, answeringModel())
	plan, _ := json.Marshal(storedPlan{Memories: []storedPlanMemory{{ID: memID, Label: "x"}}, Mode: "thought"})
	_, err := e.db.Queries.CreateMessage(context.Background(), db.CreateMessageParams{
		UserID: e.userID, SessionID: "ap-other", Role: "assistant", Content: "отговор",
		SourceChunkIds: pqtype.NullRawMessage{RawMessage: []byte("[]"), Valid: true},
		AnswerPlan:     pqtype.NullRawMessage{RawMessage: plan, Valid: true},
	})
	require.NoError(t, err)
	assert.Equal(t, map[uuid.UUID]bool{memID: true}, o.recentlyPlannedMemories(context.Background(), e.userID))
}

func TestPlanSignalsOfTheTurn(t *testing.T) {
	o := &Orchestrator{}
	s := o.planSignals(context.Background(), "запиши това във файла бележки.txt", true, true)
	assert.True(t, s.FileRequest)
	assert.True(t, s.Attachment)
	assert.True(t, s.Task)
	assert.False(t, s.Cloud, "no paid model")

	o.paid = &llm.MockLLM{}
	assert.False(t, o.planSignals(context.Background(), tiredMessage, false, false).Cloud, "the paid model only when chosen")
	assert.True(t, o.planSignals(WithLLMChoice(context.Background(), ptr(true)), tiredMessage, false, false).Cloud)
	assert.False(t, o.planSignals(WithLLMChoice(context.Background(), ptr(false)), tiredMessage, false, false).Cloud)
	assert.True(t, o.planSignals(context.Background(), "успя ли?", false, false).StatusQuestion)
	assert.True(t, o.planSignals(context.Background(), "какво пише Марк Аврелий за умората в книгата", false, false).ForcedTool)
}

func ptr[T any](v T) *T { return &v }

func TestWithoutPlanMarks(t *testing.T) {
	for _, tc := range []struct{ in, out string }{
		{"Почини си на дивана [1].", "Почини си на дивана."},
		{"Диванът [1], баницата [2] и разходката [3].", "Диванът, баницата и разходката."},
		{"[1] В началото.", " В началото."},
		{"Без знаци.", "Без знаци."},
		{"Масив [a] и [ 1 ] остават.", "Масив [a] и [ 1 ] остават."},
	} {
		got, _ := withoutPlanMarks(tc.in)
		assert.Equal(t, tc.out, got, tc.in)
	}
	_, marks := withoutPlanMarks("a [2] b [1] c [2] d [12]")
	assert.Equal(t, []int{2, 1, 2, 12}, marks)
}

func TestThePlanMarkFilterOfTheStream(t *testing.T) {
	for _, pieces := range [][]string{
		{"Почини си [1]. Заслужаваш го."},
		{"Почини си [", "1]. Заслужаваш го."},
		{"Почини си ", "[1", "]. Заслужаваш го."},
		{"Почини си", " ", "[", "1", "]", ". Заслужаваш го."},
	} {
		f := &planMarkFilter{}
		var b strings.Builder
		for _, p := range pieces {
			b.WriteString(f.Process(p))
		}
		b.WriteString(f.Flush())
		assert.Equal(t, "Почини си. Заслужаваш го.", b.String(), "%q", pieces)
	}
	// What is not a mark goes on, even split.
	f := &planMarkFilter{}
	out := f.Process("Масив [") + f.Process("a] и интервал ") + f.Process("накрая") + f.Flush()
	assert.Equal(t, "Масив [a] и интервал накрая", out)
	// A text that ends on a space or a "[" keeps it at the end.
	f = &planMarkFilter{}
	assert.Equal(t, "край [", f.Process("край [")+f.Flush())
}

func TestPlanReplyWithoutAPlanChangesNothing(t *testing.T) {
	var turn *turnState
	content, stored := turn.planReply("Текст [1].")
	assert.Equal(t, "Текст [1].", content, "a turn without a plan keeps its brackets")
	assert.False(t, stored.Valid)
	assert.Equal(t, "Текст [1].", (&turnState{}).withoutPlanMarksIfPlanned("Текст [1]."))
}

func TestPlanLabelIsTheStartOfTheMemory(t *testing.T) {
	assert.Equal(t, "Кратко.", planLabel("Кратко."))
	long := "Ивайло обича дългите разходки покрай Везер в Бремен, когато има нужда да си подреди мислите."
	label := planLabel(long)
	assert.True(t, strings.HasSuffix(label, "…"))
	assert.LessOrEqual(t, len([]rune(label)), planLabelRunes+1)
	assert.True(t, strings.HasPrefix(long, strings.TrimSuffix(label, "…")), "cut at a word")
	assert.NotContains(t, strings.TrimSuffix(label, "…"), "  ")
}

// streamedText is the text the person was sent in an SSE body: the "content" of its data frames.
func streamedText(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(data), &frame) != nil {
			continue
		}
		if s, ok := frame["content"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// A regenerated answer of a planned turn is planned again, and stored without marks, with its plan.
func TestARegeneratedAnswerOfAPlanKeepsThePlan(t *testing.T) {
	model := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Нов отговор [1].")}}
	e, o, memID := plannedEnv(t, model)
	storeTurnMessages(t, e, "ap-regen", [2]string{"user", tiredMessage}, [2]string{"assistant", "Стар отговор."})

	body, _ := json.Marshal(RegenerateRequest{SessionID: "ap-regen"})
	req := httptest.NewRequest(http.MethodPost, "/chat/regenerate", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleRegenerate(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "ap-regen", Limit: 10})
	require.NoError(t, err)
	require.Len(t, stored, 2)
	assert.Equal(t, "Нов отговор.", stored[0].Content)
	require.True(t, stored[0].AnswerPlan.Valid)
	var p storedPlan
	require.NoError(t, json.Unmarshal(stored[0].AnswerPlan.RawMessage, &p))
	require.Len(t, p.Memories, 1)
	assert.Equal(t, memID, p.Memories[0].ID)
}
