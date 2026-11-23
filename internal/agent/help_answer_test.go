package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/metrics"
)

func TestAsksForHelp(t *testing.T) {
	yes := []string{
		"какви инструменти имаш и как да ги извикам?",
		"напомни ми какво можеш да правиш и какво да ти пиша за всяко нещо",
		"Какво можеш?",
		"какво още умееш",
		"какви са ти възможностите",
		"как да те използвам?",
		"кажи ми какво можеш",
		"с какво можеш да ми помогнеш?",
		"помощ",
		"/help",
		"What can you do?",
		"what tools do you have",
	}
	no := []string{
		"какво можеш да ми кажеш за стоицизма?",
		"можеш ли да ми запишеш това във файл",
		"какво можеш да направиш с този код: func main() {}",
		"какви инструменти има в книгата за Docker?" + strings.Repeat(" и още много думи", 10),
		"помощ ми трябва с кода",
		"Прочети https://example.org и ми кажи какви са възможностите на модела",
		"уморен съм",
		"какво правиш?",
		"",
	}
	for _, m := range yes {
		assert.True(t, asksForHelp(m), m)
	}
	for _, m := range no {
		assert.False(t, asksForHelp(m), m)
	}
	assert.True(t, asksForHelp("какво можеш?"+closingNoteStatement), "the note of the turn does not count")
}

// The list follows what is on: a switch that is off takes its lines out.
func TestTheHelpTextSaysOnlyWhatIsOn(t *testing.T) {
	plain := (&Orchestrator{}).helpText()
	for _, always := range []string{"Книгите ти", "„Какво казва Сенека за гнева?“", "Интернет", "Паметта", "„Запомни, че …“", "Как да говорим"} {
		assert.Contains(t, plain, always)
	}
	for _, off := range []string{"Не разбирам какво е", "Библиотеката", "Моите файлове", "Успя ли?", "Прикачи файл", "облак / вкъщи"} {
		assert.NotContains(t, plain, off, "off: "+off)
	}

	all := &Orchestrator{addToLibrary: true, explainDossier: true, statusAnswer: true, paid: &llm.MockLLM{}}
	all.WithFileTools()
	text := all.helpText()
	for _, on := range []string{"Не разбирам какво е", "под заглавие", "Запиши цялата статия във файл", "Успя ли?", "„Запази в библиотеката“", "облак / вкъщи"} {
		assert.Contains(t, text, on, "on: "+on)
	}

	noFiles := &Orchestrator{statusAnswer: true}
	assert.NotContains(t, noFiles.helpText(), "Успя ли?", "the status is about the files")
	filesOnly := (&Orchestrator{}).WithFileTools()
	assert.Contains(t, filesOnly.helpText(), "Моите файлове")
	assert.NotContains(t, filesOnly.helpText(), "Успя ли?", "the status answer is off")
}

func TestTheHelpAnswerIsOffByDefault(t *testing.T) {
	o := &Orchestrator{}
	assert.Empty(t, o.helpAnswerFor("какво можеш?"))
	o.WithHelpAnswer()
	before := counterValue(t, metrics.HelpAnswers)
	assert.NotEmpty(t, o.helpAnswerFor("какво можеш?"))
	assert.Empty(t, o.helpAnswerFor("уморен съм"))
	assert.Equal(t, before+1, counterValue(t, metrics.HelpAnswers))
}

// The whole turn: the code answers, the model is not asked, the answer is stored.
func TestAQuestionAboutWhatSheCanDoIsAnsweredByTheCode(t *testing.T) {
	e := newAttachmentEnv(t)
	model := answeringModel()
	o := orchestratorWith(t, e, model).WithFileTools().WithHelpAnswer()

	body, _ := json.Marshal(ChatRequest{SessionID: "help-1", Content: "какви инструменти имаш и как да ги извикам?"})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Contains(t, rec.Body.String(), "Ето какво мога сега")
	assert.Contains(t, rec.Body.String(), "data: [DONE]")
	assert.Empty(t, model.Requests, "the model was not asked")
	stored, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "help-1", Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, stored)
	assert.Equal(t, "assistant", stored[0].Role)
	assert.Contains(t, stored[0].Content, "Моите файлове", "the stored answer is the list, with the files that are on")
}
