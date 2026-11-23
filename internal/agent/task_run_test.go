package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
	"go-monolith/internal/userfiles"
)

func TestWhichMessagesAskTheServerForATaskOverManyDocuments(t *testing.T) {
	cases := map[string]string{
		"сега за всеки от добавените източници направи отделен файл с ключовите му факти, за да ги ползвам при стратегии": taskrun.KindKeyFacts,
		"за всяка от статиите запиши във файл основните изводи":                                                           taskrun.KindKeyFacts,
		"направи резюме на всеки от документите в отделен файл":                                                           taskrun.KindSummary,
		"make a file with the key facts for each paper":                  taskrun.KindKeyFacts,
		"за всеки от източниците обобщи ключовите факти тук в чата":      "", // no file: an answer in the chat
		"направи файл с ключовите факти на тази статия":                  "", // one document
		"за всеки случай запиши във файл ключовите факти":                "", // "just in case"
		"за всеки от добавените източници направи отделен файл с превод": "", // a recipe the server has not
	}
	for message, want := range cases {
		assert.Equal(t, want, taskRunKind(message), message)
	}
}

func TestTheShortAnswersOfThePerson(t *testing.T) {
	for _, yes := range []string{"да", "Да!", "давай", "Започни", "ок", "добре, започвай", "yes", "да, пускай"} {
		assert.True(t, isYes(yes), yes)
	}
	for _, notYes := range []string{"да не започваш още", "не", "да, но само първите пет и после ми кажи какво става с останалите", "дай ми първо списъка", ""} {
		assert.False(t, isYes(notYes), notYes)
	}
	for _, no := range []string{"не", "Не, недей", "няма нужда", "откажи"} {
		assert.True(t, isNo(no), no)
	}
	assert.False(t, isNo("неща"))
	for _, stop := range []string{"спри", "Спри!", "стоп", "прекрати задачата", "откажи задачата", "stop"} {
		assert.True(t, isStop(stop), stop)
	}
	for _, notStop := range []string{"спринт", "не спирай да ми пишеш, когато свършиш всичко с файловете на всички тези статии"} {
		assert.False(t, isStop(notStop), notStop)
	}
}

type countingWaker struct{ n int }

func (w *countingWaker) Wake() { w.n++ }

type taskRunEnv struct {
	*attachmentEnv
	o      *Orchestrator
	model  *llm.MockLLM
	plans  *taskplan.Store
	waker  *countingWaker
	others []string
}

func newTaskRunEnv(t *testing.T) *taskRunEnv {
	t.Helper()
	e := newAttachmentEnv(t)
	store, err := userfiles.Open(filepath.Join(t.TempDir(), "files"), userfiles.Limits{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	e.reg.WithFiles(store)
	model := answeringModel()
	plans := taskplan.NewStore(e.db)
	waker := &countingWaker{}
	o := orchestratorWith(t, e, model).WithFileTools().WithStatusAnswer().WithTaskRun(plans, waker)
	return &taskRunEnv{attachmentEnv: e, o: o, model: model, plans: plans, waker: waker}
}

func (e *taskRunEnv) addDocument(t *testing.T, name, age string) db.IngestedFile {
	t.Helper()
	file, err := e.db.Queries.CreateIngestedFile(context.Background(), db.CreateIngestedFileParams{
		UserID: e.userID, FileName: name, FileHash: "h-" + uuid.NewString(), MimeType: "application/pdf", FileSize: 1, Status: "pending", LocalPath: "x",
	})
	require.NoError(t, err)
	if age != "" {
		_, err := e.db.SqlDB.Exec(`UPDATE ingested_files SET created_at = now() - $2::interval WHERE id = $1`, file.ID, age)
		require.NoError(t, err)
	}
	return file
}

func (e *taskRunEnv) say(t *testing.T, session, message string) string {
	t.Helper()
	body, _ := json.Marshal(ChatRequest{SessionID: session, Content: message})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	e.o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

const askForFacts = "сега за всеки от добавените източници направи отделен файл с ключовите му факти"

func TestTheTaskIsProposedStartedAskedAboutAndStopped(t *testing.T) {
	e := newTaskRunEnv(t)
	e.addDocument(t, "old-book.pdf", "2 days")
	storeTurnMessages(t, e.attachmentEnv, "tr", [2]string{"user", "добави тези в библиотеката: https://example.org/a.pdf https://example.org/b.pdf"},
		[2]string{"assistant", "Добавих ги."})
	e.addDocument(t, "paper-a.pdf", "")
	e.addDocument(t, "paper-b.pdf", "")

	out := e.say(t, "tr", askForFacts)
	assert.Contains(t, out, "за всеки от тези 2 документа")
	assert.Contains(t, out, "• paper-a.pdf")
	assert.Contains(t, out, "• paper-b.pdf")
	assert.NotContains(t, out, "old-book.pdf", "only the documents added in the conversation")
	assert.Contains(t, out, "Да започна ли?")
	assert.Empty(t, e.model.Requests, "the model was not asked")
	proposed, err := e.plans.Proposed(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	require.NotNil(t, proposed)
	assert.Equal(t, taskrun.KindKeyFacts, proposed.Kind)
	require.Len(t, proposed.Steps, 2)
	assert.Equal(t, "paper-a.pdf", proposed.Steps[0].Text, "the oldest first")
	assert.Equal(t, 0, e.waker.n, "nothing starts before the yes")

	out = e.say(t, "tr", "да")
	assert.Contains(t, out, "Започнах.")
	assert.Equal(t, 1, e.waker.n, "the runner is woken")
	running, err := e.plans.Running(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	require.NotNil(t, running)
	assert.Equal(t, proposed.ID, running.ID)

	out = e.say(t, "tr", "докъде стигна?")
	assert.Contains(t, out, "готови 0 от 2 стъпки")
	assert.Contains(t, out, "Следва: 1. paper-a.pdf")

	out = e.say(t, "tr", "да")
	assert.Contains(t, out, "Вече работя по нея.", "a second yes starts nothing")

	out = e.say(t, "tr", "спри")
	assert.Contains(t, out, "Спрях задачата.")
	running, err = e.plans.Running(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	assert.Nil(t, running)
	assert.Empty(t, e.model.Requests, "no turn of the task asked the model")
}

func TestAnotherMessageDropsTheProposalAndTheModelAnswers(t *testing.T) {
	e := newTaskRunEnv(t)
	storeTurnMessages(t, e.attachmentEnv, "tr", [2]string{"user", "добави ги"}, [2]string{"assistant", "Добавих ги."})
	e.addDocument(t, "paper-a.pdf", "")

	e.say(t, "tr", askForFacts)
	e.say(t, "tr", "всъщност какво е нощна доходност?")
	assert.NotEmpty(t, e.model.Requests, "the model answers the other question")
	proposed, err := e.plans.Proposed(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	assert.Nil(t, proposed, "the proposal is dropped")

	e.say(t, "tr", "да")
	assert.Equal(t, 0, e.waker.n, "a later yes starts nothing")
	running, err := e.plans.Running(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	assert.Nil(t, running)
}

func TestANoDropsTheProposal(t *testing.T) {
	e := newTaskRunEnv(t)
	storeTurnMessages(t, e.attachmentEnv, "tr", [2]string{"user", "добави ги"}, [2]string{"assistant", "Добавих ги."})
	e.addDocument(t, "paper-a.pdf", "")
	e.say(t, "tr", askForFacts)

	out := e.say(t, "tr", "не, недей")
	assert.Contains(t, out, "Добре, няма да го правя.")
	assert.Empty(t, e.model.Requests)
	proposed, err := e.plans.Proposed(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	assert.Nil(t, proposed)
}

func TestWithoutAddedDocumentsTheModelAnswers(t *testing.T) {
	e := newTaskRunEnv(t)
	e.addDocument(t, "old-book.pdf", "2 days")
	storeTurnMessages(t, e.attachmentEnv, "tr", [2]string{"user", "здрасти"}, [2]string{"assistant", "Здравей!"})

	e.say(t, "tr", askForFacts)

	assert.NotEmpty(t, e.model.Requests, "the model answers")
	proposed, err := e.plans.Proposed(context.Background(), e.userID, "tr")
	require.NoError(t, err)
	assert.Nil(t, proposed)
}

func TestTheTaskOfAnotherConversationIsNotThisOne(t *testing.T) {
	e := newTaskRunEnv(t)
	storeTurnMessages(t, e.attachmentEnv, "one", [2]string{"user", "добави ги"}, [2]string{"assistant", "Добавих ги."})
	e.addDocument(t, "paper-a.pdf", "")
	e.say(t, "one", askForFacts)

	storeTurnMessages(t, e.attachmentEnv, "two", [2]string{"user", "здрасти"}, [2]string{"assistant", "Здравей!"})
	e.say(t, "two", "да")

	assert.Equal(t, 0, e.waker.n, "a yes in another conversation starts nothing")
	proposed, err := e.plans.Proposed(context.Background(), e.userID, "one")
	require.NoError(t, err)
	assert.NotNil(t, proposed)
}

func TestWhileATaskRunsSayingThatWorkGoesOnIsNoStall(t *testing.T) {
	var stall answerCheck
	for _, c := range answerChecks {
		if c.name == "stall" {
			stall = c
		}
	}
	require.NotNil(t, stall.note)
	_, turn := withTurn(context.Background(), "s", false)
	facts := answerFacts{content: "В момента работя по файловете, когато свърша ще ти кажа.", turn: turn}
	assert.Equal(t, stallCorrection, stall.note(context.Background(), nil, facts), "without a task it is a promise no one keeps")
	turn.markTaskRunning()
	assert.Empty(t, stall.note(context.Background(), nil, facts), "with a task of the server it is true")
}

func TestTheHelpListNamesTheTaskWhenItIsOn(t *testing.T) {
	e := newTaskRunEnv(t)
	e.o.WithLibraryAdd()
	assert.Contains(t, e.o.helpText(), "ключовите му факти")
	plain := orchestratorWith(t, e.attachmentEnv, answeringModel()).WithFileTools().WithLibraryAdd()
	assert.NotContains(t, plain.helpText(), "ключовите му факти")
}
