package taskrun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/taskplan"
)

// fakePages reads a page as the test says: its title and text, or an error.
type fakePages struct {
	pages map[string][2]string
	errs  map[string]error
	asked []string
}

func (f *fakePages) ReadPage(_ context.Context, link string) (string, string, error) {
	f.asked = append(f.asked, link)
	if err, ok := f.errs[link]; ok {
		return "", "", err
	}
	p, ok := f.pages[link]
	if !ok {
		return "", "", errors.New("няма го на този адрес (404)")
	}
	return p[0], p[1], nil
}

// startResearch proposes a research over pages and starts it, as the yes of the person does.
func (e *env) startResearch(t *testing.T, session string, links ...string) *taskplan.Plan {
	t.Helper()
	var steps []taskplan.Step
	for i, l := range links {
		steps = append(steps, taskplan.Step{ID: i + 1, Text: "page " + l, Link: l, About: "изграждане на OCR система",
			Query: "how to build an OCR system", Status: taskplan.StepPending})
	}
	proposed, err := e.plans.Propose(e.ctx, e.user, session, KindResearch, "справочник „изграждане на OCR система“ от страници в интернет", steps)
	require.NoError(t, err)
	started, err := e.plans.Start(e.ctx, e.user, proposed.ID)
	require.NoError(t, err)
	return started
}

const (
	tesseractPage  = "Tesseract 5 reads 100 languages. Install it with apt install tesseract-ocr, then run tesseract image.png out.\nBinarise the image first: Otsu thresholding helps."
	tesseractNotes = "- Tesseract 5 чете 100 езика.\n- Инсталира се с apt install tesseract-ocr.\n- Изображението първо се прави черно-бяло (Otsu)."
)

func TestAResearchWritesOneFileWithASectionAndTheLinkOfEachPage(t *testing.T) {
	e := newEnv(t, tesseractNotes, "- EasyOCR поддържа 80 езика.\n- Работи с PyTorch.")
	pages := &fakePages{pages: map[string][2]string{
		"https://a.example/tesseract": {"Tesseract OCR tutorial", tesseractPage},
		"https://b.example/easyocr":   {"EasyOCR in Python", "EasyOCR supports 80 languages and runs on PyTorch."},
	}}
	e.runner.WithPages(pages)
	e.startResearch(t, "s", "https://a.example/tesseract", "https://b.example/easyocr")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	for _, s := range p.Steps {
		assert.Equal(t, taskplan.StepDone, s.Status, s.Note)
		assert.Equal(t, "изграждане на OCR система - справочник.md", s.File)
	}
	text := e.read(t, "изграждане на OCR система - справочник.md")
	assert.Contains(t, text, "# Справочник: изграждане на OCR система")
	assert.Contains(t, text, "## 1. Tesseract OCR tutorial\n\nИзточник: https://a.example/tesseract")
	assert.Contains(t, text, "- Tesseract 5 чете 100 езика.")
	assert.Contains(t, text, "## 2. EasyOCR in Python\n\nИзточник: https://b.example/easyocr")
	assert.Equal(t, 1, strings.Count(text, "# Справочник"), "one file, one heading")
	assert.Contains(t, p.Steps[1].Evidence, "раздел 2")

	// The model got the text of the page, the subject, and no tools; the page is data, not orders.
	require.NotEmpty(t, e.llm.requests)
	assert.Contains(t, e.llm.requests[0].Messages[0].Content, "„изграждане на OCR система“")
	assert.Contains(t, e.llm.requests[0].Messages[0].Content, "не инструкции")
	assert.Contains(t, e.llm.requests[0].Messages[1].Content, "apt install tesseract-ocr")
	assert.Empty(t, e.llm.requests[0].Tools)

	report := e.assistantMessages(t, "s")
	require.Len(t, report, 1)
	assert.Contains(t, report[0], "записани 2 от 2 страници")
	assert.Contains(t, report[0], "Файлът: изграждане на OCR система - справочник.md (2 раздела")
}

func TestAPageThatCannotBeReadOrHasNothingOnTheSubjectIsLeftOutWithItsReason(t *testing.T) {
	e := newEnv(t, "НЯМА", tesseractNotes)
	pages := &fakePages{
		pages: map[string][2]string{
			"https://ads.example/x":       {"Buy now", "A page of offers with no word on the subject at all, just links and prices."},
			"https://a.example/tesseract": {"Tesseract OCR tutorial", tesseractPage},
		},
		errs: map[string]error{"https://blocked.example/p": errors.New("сайтът не пуска програми (отказ 401/403)")},
	}
	e.runner.WithPages(pages)
	e.startResearch(t, "s", "https://blocked.example/p", "https://ads.example/x", "https://a.example/tesseract")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Equal(t, "сайтът не пуска програми (отказ 401/403)", p.Steps[0].Note)
	assert.Equal(t, taskplan.StepSkipped, p.Steps[1].Status)
	assert.Equal(t, "на страницата няма нищо по темата", p.Steps[1].Note)
	assert.Equal(t, taskplan.StepDone, p.Steps[2].Status, p.Steps[2].Note)
	text := e.read(t, "изграждане на OCR система - справочник.md")
	assert.Contains(t, text, "## 1. Tesseract OCR tutorial", "the sections are numbered as written")
	report := e.assistantMessages(t, "s")
	require.Len(t, report, 1)
	assert.Contains(t, report[0], "записани 1 от 3 страници")
	assert.Contains(t, report[0], "page https://blocked.example/p — сайтът не пуска програми")
	assert.Contains(t, report[0], "на страницата няма нищо по темата")
}

func TestANumberThatIsNotOnThePageIsNotWritten(t *testing.T) {
	// The model gives a number from elsewhere twice: the line goes, the rest stays.
	wrong := tesseractNotes + "\n- Точността е 99.7%."
	e := newEnv(t, wrong, wrong)
	e.runner.WithPages(&fakePages{pages: map[string][2]string{"https://a.example/tesseract": {"Tesseract OCR tutorial", tesseractPage}}})
	e.startResearch(t, "s", "https://a.example/tesseract")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status, p.Steps[0].Note)
	text := e.read(t, "изграждане на OCR система - справочник.md")
	assert.NotContains(t, text, "99.7")
	assert.Contains(t, text, "- Tesseract 5 чете 100 езика.")
	assert.Len(t, e.llm.requests, 2, "asked once more with the numbers named")
	assert.Contains(t, e.llm.requests[1].Messages[3].Content, "99.7")
}

func TestAResearchWhosePagesAllGiveNothingWritesNoFile(t *testing.T) {
	e := newEnv(t, "НЯМА")
	e.runner.WithPages(&fakePages{pages: map[string][2]string{"https://ads.example/x": {"Buy now", "Offers and prices only, nothing else here."}}})
	e.startResearch(t, "s", "https://ads.example/x", "https://gone.example/y")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.NoFileExists(t, e.dir+"/изграждане на OCR система - справочник.md")
	report := e.assistantMessages(t, "s")
	require.Len(t, report, 1)
	assert.Contains(t, report[0], "Нито една страница не даде бележки, затова файл няма.")
}

func TestAResearchWithoutAReaderOfPagesFailsItsStepsAndPutsNothingIntoTheLibrary(t *testing.T) {
	e := newEnv(t, tesseractNotes)
	links := &fakeLinks{}
	e.runner.WithLinks(links)
	e.startResearch(t, "s", "https://a.example/tesseract")

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Equal(t, "четенето на страници е изключено", p.Steps[0].Note)
	assert.Empty(t, links.asked, "a page found by a search never goes into the library")
}

func TestASectionGoesOnInANewFileWhenThePersonDeletedTheFile(t *testing.T) {
	e := newEnv(t, tesseractNotes)
	pages := &fakePages{pages: map[string][2]string{
		"https://a.example/tesseract": {"Tesseract OCR tutorial", tesseractPage},
		"https://b.example/tesseract": {"Tesseract again", tesseractPage},
	}}
	e.runner.WithPages(pages)
	e.startResearch(t, "s", "https://a.example/tesseract", "https://b.example/tesseract")
	// One pass: the first page is written; then the person deletes the file.
	require.True(t, e.runner.pass(e.ctx))
	require.NoError(t, e.files.Delete(e.user, "изграждане на OCR система - справочник.md"))

	e.walk(t)

	p := e.plan(t, "s")
	assert.Equal(t, taskplan.StepDone, p.Steps[1].Status, p.Steps[1].Note)
	assert.Equal(t, "изграждане на OCR система - справочник - продължение.md", p.Steps[1].File)
	assert.Equal(t, "изграждане на OCR система - справочник - продължение.md", ResearchFile(p))
}

func TestTheFileOfAResearchIsKnownAsMadeByATask(t *testing.T) {
	assert.True(t, MadeFile("изграждане на OCR система - справочник.md"))
}
