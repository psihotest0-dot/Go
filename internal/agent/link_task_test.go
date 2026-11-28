package agent

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
	"go-monolith/internal/webfetch"
)

func TestLinkTaskKind(t *testing.T) {
	cases := []struct {
		message, kind string
		named         bool
	}{
		// The Flet chat of 2026-11-20, word for word.
		{"искам да пробваш да извлечеш информация от посочените линкове и за всеки да създадеш файл с информация която да се ползва за бъдещо оформяне на стратегии", taskrun.KindKeyFacts, false},
		{"за всеки линк направи файл с ключовите факти", taskrun.KindKeyFacts, true},
		{"за всеки от тези линкове направи файл с резюме", taskrun.KindSummary, true},
		{"за всеки линк запиши цялата статия във файл", taskrun.KindFull, true},
		{"за всеки от тези направи отделен файл с резюме: https://a.example/x.pdf https://b.example/y", taskrun.KindSummary, true},
	}
	for _, c := range cases {
		kind, named := linkTaskKind(c.message)
		assert.Equal(t, c.kind, kind, c.message)
		assert.Equal(t, c.named, named, c.message)
	}
	for _, m := range []string{
		"за всеки от добавените източници направи отделен файл с ключовите му факти", // the documents in the library (task_run.go)
		"за всеки от добавените линкове направи файл с ключовите факти",              // the same, with the links named
		"за всеки линк ми кажи какво е и колко е голям",                              // a question (link_probe.go)
		"добави линковете в библиотеката",                                            // one call of add_to_library
		"за всеки файл в папката направи резюме",                                     // no links
	} {
		kind, _ := linkTaskKind(m)
		assert.Empty(t, kind, m)
	}
}

// linkTaskEnv is the task-run env with add_to_library, and links that the fake fetcher gives.
func linkTaskEnv(t *testing.T) (*taskRunEnv, *fakeFetcher, *fakeAdder) {
	t.Helper()
	e := newTaskRunEnv(t)
	f := &fakeFetcher{
		docs: map[string]*webfetch.Document{
			"https://a.example/paper.pdf": {Name: "paper.pdf", Kind: webfetch.KindPDF, Body: bytes.NewReader([]byte("%PDF-1.4 paper"))},
		},
		errs: map[string]error{"https://c.example/abstract": &webfetch.Error{Reason: webfetch.ReasonAbstractOnly}},
	}
	adder := &fakeAdder{}
	e.reg.WithLibraryAdd(f, adder)
	e.o.WithLibraryAdd()
	return e, f, adder
}

const linksMessage = "ето източниците: https://a.example/paper.pdf https://c.example/abstract"

func TestATaskOverTheLinksIsProposedWithWhatEachOneIs(t *testing.T) {
	e, _, adder := linkTaskEnv(t)
	e.say(t, "s", linksMessage)
	asked := len(e.model.Requests)

	out := streamedText(t, e.say(t, "s", "за всеки линк създай файл с информация за стратегии"))

	assert.Equal(t, asked, len(e.model.Requests), "the code answers, the model is not asked")
	assert.Contains(t, out, "Мога да сваля линковете в библиотеката един по един")
	assert.Contains(t, out, "• paper.pdf — PDF")
	assert.Contains(t, out, "Не стават (1)")
	assert.Contains(t, out, "c.example — страницата има само резюме")
	assert.Contains(t, out, "Да започна ли?")
	assert.Contains(t, out, "кажи „цялата“", "no recipe was named: the key facts, and how to get the whole")
	assert.Empty(t, adder.added, "nothing is added before the yes")

	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, taskrun.KindKeyFacts, p.Kind)
	require.Len(t, p.Steps, 1, "only the link that gives a document")
	assert.Equal(t, "https://a.example/paper.pdf", p.Steps[0].Link)
	assert.Empty(t, p.Steps[0].Ref)

	start := streamedText(t, e.say(t, "s", "да"))
	assert.Contains(t, start, "Започнах")
	running, err := e.plans.Running(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, running)
	assert.Equal(t, taskrun.KindKeyFacts, running.Kind)
	assert.Equal(t, 1, e.waker.n, "the runner is woken")
}

func TestWholeToTheProposalStartsTheNotesOverTheWholeText(t *testing.T) {
	e, _, _ := linkTaskEnv(t)
	e.say(t, "s", linksMessage)
	e.say(t, "s", "за всеки линк създай файл с информация")

	out := streamedText(t, e.say(t, "s", "цялата"))

	assert.Contains(t, out, "Започнах")
	assert.Contains(t, out, "целия текст на части")
	running, err := e.plans.Running(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, running)
	assert.Equal(t, taskrun.KindFull, running.Kind)
	assert.Equal(t, "подробни бележки по целия текст на всеки от 1 линка", running.Goal)
	assert.Equal(t, "https://a.example/paper.pdf", running.Steps[0].Link, "the same steps")
}

func TestATaskOverLinksWithoutLinksSaysWhereToPutThem(t *testing.T) {
	e, _, _ := linkTaskEnv(t)
	out := streamedText(t, e.say(t, "s", "за всеки линк създай файл с информация"))
	assert.Contains(t, out, "Не виждам линкове")
	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Nil(t, p)
}

func TestLinksThatAllGiveNothingStartNothing(t *testing.T) {
	e, _, _ := linkTaskEnv(t)
	e.say(t, "s", "ето: https://c.example/abstract")
	out := streamedText(t, e.say(t, "s", "за всеки линк създай файл с информация"))
	assert.Contains(t, out, "нищо не започвам")
	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Nil(t, p)
}

func TestWithoutLibraryAddTheTaskOverLinksIsNotTheCodes(t *testing.T) {
	e := newTaskRunEnv(t)
	e.say(t, "s", linksMessage)
	out := streamedText(t, e.say(t, "s", "за всеки линк създай файл с информация"))
	assert.NotContains(t, out, "Мога да сваля линковете")
}

func TestAddLinkPutsTheDocumentIntoTheLibraryAndSaysWhyNot(t *testing.T) {
	f := &fakeFetcher{
		docs: map[string]*webfetch.Document{"https://a.example/paper.pdf": pdfDoc("paper.pdf")},
		errs: map[string]error{
			"https://d.example/blocked":  &webfetch.Error{Reason: webfetch.ReasonBlocked},
			"https://e.example/huge.pdf": webfetch.ErrTooLarge,
		},
	}
	adder := &fakeAdder{dup: map[string]bool{}}
	r := addTool(f, adder)

	id, name, err := r.AddLink(context.Background(), uuid.New(), "https://a.example/paper.pdf")
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, id)
	assert.Equal(t, "paper.pdf", name)
	assert.Contains(t, adder.added, "paper.pdf")

	_, _, err = r.AddLink(context.Background(), uuid.New(), "https://d.example/blocked")
	require.Error(t, err)
	assert.Equal(t, "сайтът не пуска програми (отказ 401/403)", err.Error())
	_, _, err = r.AddLink(context.Background(), uuid.New(), "https://e.example/huge.pdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "по-голям от")

	_, _, err = (&DefaultToolRegistry{}).AddLink(context.Background(), uuid.New(), "https://a.example/paper.pdf")
	assert.EqualError(t, err, "свалянето на линкове е изключено")
}

func TestALinkAlreadyInTheLibraryIsAStepOnItsDocument(t *testing.T) {
	probes := []linkProbe{{Link: "https://a.example/p.pdf", Title: "Paper", Kind: webfetch.KindPDF, Bytes: 2048, InLibrary: "Paper.pdf", InLibraryID: uuid.New()}}
	text := linkProposalText(taskrun.KindKeyFacts, true, probes, false, 1)
	assert.Contains(t, text, "вече е в библиотеката като „Paper.pdf“ — не го свалям пак")
	assert.NotContains(t, text, "кажи „цялата“", "the recipe was named")
	_ = taskplan.StepPending
}

func TestTheHelpListNamesTheTaskOverLinks(t *testing.T) {
	e, _, _ := linkTaskEnv(t)
	assert.Contains(t, e.o.helpText(), "За всеки линк направи файл")
	plain := orchestratorWith(t, e.attachmentEnv, answeringModel()).WithFileTools().WithLibraryAdd()
	assert.NotContains(t, plain.helpText(), "За всеки линк направи файл", "not without the tasks of the server")
}
