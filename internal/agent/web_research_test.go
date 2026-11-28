package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/internal/taskrun"
	"go-monolith/internal/webfetch"
)

// The Telegram turns of 2026-11-24, word for word.
const (
	researchAsk      = "Опитай да направиш справочник от примерите за проекти във твоята папка с файлове ,който би послужил за изграждане на успешен OCR проект"
	researchFollowUp = "От интернет имах предвид да,започни"
)

func TestAsksForResearch(t *testing.T) {
	for _, c := range []struct{ message, previous string }{
		{researchFollowUp, researchAsk},
		{"намери в интернет статии за изграждане на OCR и направи справочник във файл", ""},
		{"потърси онлайн за грижата за орхидеи и ми състави бележки", ""},
		{"find articles on the internet about OCR and write a guide to a file", ""},
	} {
		assert.True(t, asksForResearch(c.message, c.previous), c.message)
	}
	for _, c := range []struct{ message, previous string }{
		{"Опитай да намериш в интернет свободна статия или примери за изграждане на ocr", ""}, // a search, no file (#158)
		{researchAsk, ""}, // the folder, not the internet: the model looks there
		{"От интернет имах предвид да,започни", "Опитай да намериш в интернет свободна статия"}, // nothing to make
		{"намери в интернет и направи файл: https://a.example/x", ""},                           // a link the person gave
		{"според книгите и новините направи бележки за инфлацията във файл", ""},                // the books (books_web.go)
		{"потърси в интернет и добави в библиотеката статии за OCR", ""},                        // the library
		{"от интернет", "направи справочник от тези линкове: https://a.example/x"},              // the links of the person
	} {
		assert.False(t, asksForResearch(c.message, c.previous), c.message+" || "+c.previous)
	}
}

func TestPreviousUserMessage(t *testing.T) {
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "първо"},
		{Role: openai.ChatMessageRoleAssistant, Content: "отговор"},
		{Role: openai.ChatMessageRoleUser, Content: "второ"},
		{Role: openai.ChatMessageRoleAssistant, Content: "отговор"},
		{Role: openai.ChatMessageRoleUser, Content: "сегашно"},
	}
	assert.Equal(t, "второ", previousUserMessage(history))
	assert.Empty(t, previousUserMessage(history[:1]))
}

func TestResearchPagesAreReadableOnesOfDifferentSites(t *testing.T) {
	hits := []webHit{
		{Title: "Video", Link: "https://www.youtube.com/watch?v=1"},
		{Title: "A", Link: "https://a.example/ocr"},
		{Title: "A again", Link: "https://www.a.example/ocr-2"},
		{Title: "Local", Link: "http://localhost/x"},
		{Title: "B", Link: "https://b.example/ocr"},
		{Title: "C", Link: "https://c.example/ocr"},
		{Title: "D", Link: "https://d.example/ocr"},
		{Title: "E", Link: "https://e.example/ocr"},
		{Title: "F", Link: "https://f.example/ocr"},
	}
	var got []string
	for _, h := range researchPages(hits) {
		got = append(got, h.Title)
	}
	assert.Equal(t, []string{"A", "B", "C", "D", "E"}, got)
}

func TestResearchPagesLeaveOutTheSameArticleTwiceAForeignScriptAndTheFragment(t *testing.T) {
	// The search of 2026-11-28, cut down.
	hits := []webHit{
		{Title: "How to Extract Text Using DeepSeek-OCR - نُشر في 2025-10-22 ضمن وكيل الذكاء", Link: "https://skywork.ai/blog/x?lang=ar#1"},
		{Title: "架构解密｜一步步打造高可用的 JOCR OCR 识别服务 - javpower", Link: "https://cloud.tencent.com.cn/developer/article/2541748#1"},
		{Title: "Tesseract OCR como API o servicio", Link: "https://tesseractocr.org/es/api/#main-content"},
		{Title: "Tesseract OCR como API o servicio", Link: "https://mirror.example/es/api/"},
		{Title: "Building an OCR pipeline", Link: "https://builder.aws.com/content/x#1"},
	}
	var got []string
	for _, h := range researchPages(hits) {
		got = append(got, h.Link)
	}
	assert.Equal(t, []string{"https://tesseractocr.org/es/api/", "https://builder.aws.com/content/x"}, got)
	assert.False(t, foreignScript("Tesseract OCR – урок на български"))
	assert.False(t, foreignScript("DeepSeek-OCR 2025"))
}

// researchReader is the model of a research env: it reads the request as the test says and keeps
// what it was given; the rest is the mock of the env.
type researchReader struct {
	*llm.MockLLM
	answer string
	fail   bool
	read   [][]openai.ChatCompletionMessage
}

func (m *researchReader) ChatWithJSONSchema(_ context.Context, req openai.ChatCompletionRequest, _ interface{}) (openai.ChatCompletionResponse, error) {
	m.read = append(m.read, req.Messages)
	if m.fail {
		return openai.ChatCompletionResponse{}, errors.New("down")
	}
	return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: m.answer}}}}, nil
}

// researchEnv is the task-run env with the local fetch of pages, a search of its own and the model's
// reading of the request.
func researchEnv(t *testing.T, results string) (*taskRunEnv, *fakePages, *int, *researchReader) {
	t.Helper()
	e := newTaskRunEnv(t)
	searches := 0
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(results))
	}))
	t.Cleanup(web.Close)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	e.reg.webBaseURL, e.reg.httpClient = web.URL, web.Client()
	pages := &fakePages{title: "page", text: strings.Repeat("Tesseract reads text. ", 20)}
	e.reg.WithLocalWebFetch(pages)
	reader := &researchReader{MockLLM: e.model, answer: `{"topic": "изграждане на OCR система", "query": "how to build an OCR system tutorial"}`}
	e.o.llmClient = reader
	return e, pages, &searches, reader
}

const ocrResults = `{"results":[` +
	`{"title":"Build your own OCR with Tesseract","url":"https://a.example/tesseract","site_name":"A Blog"},` +
	`{"title":"OCR in 10 minutes","url":"https://www.youtube.com/watch?v=x","site_name":"YouTube"},` +
	`{"title":"EasyOCR tutorial","url":"https://b.example/easyocr"}]}`

func TestAResearchIsProposedWithThePagesTheSearchFound(t *testing.T) {
	e, pages, searches, reader := researchEnv(t, ocrResults)
	e.say(t, "s", researchAsk)
	asked := len(e.model.Requests)

	out := streamedText(t, e.say(t, "s", researchFollowUp))

	assert.Equal(t, asked, len(e.model.Requests), "the code answers, the turn is not streamed by the model")
	assert.Equal(t, 1, *searches)
	assert.Contains(t, out, "Потърсих в интернет „how to build an OCR system tutorial“")
	assert.Contains(t, out, "един файл „изграждане на OCR система - справочник.md“")
	assert.Contains(t, out, "• Build your own OCR with Tesseract — A Blog")
	assert.Contains(t, out, "• EasyOCR tutorial — b.example")
	assert.NotContains(t, out, "YouTube", "a video gives no text")
	assert.Contains(t, out, "чак след твоето „да“")
	assert.Contains(t, out, "Да започна ли?")
	assert.Zero(t, pages.calls, "no page is opened before the yes")
	// The model read the request: the message before it too.
	require.Len(t, reader.read, 1)
	read := reader.read[0]
	require.Len(t, read, 3)
	assert.Contains(t, read[1].Content, "OCR проект")
	assert.Equal(t, researchFollowUp, read[2].Content)

	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, taskrun.KindResearch, p.Kind)
	assert.Equal(t, "справочник „изграждане на OCR система“ от страници в интернет", p.Goal)
	require.Len(t, p.Steps, 2)
	assert.Equal(t, "https://a.example/tesseract", p.Steps[0].Link)
	assert.Equal(t, "изграждане на OCR система", p.Steps[0].About)
	assert.Equal(t, "how to build an OCR system tutorial", p.Steps[0].Query)
	assert.Empty(t, p.Steps[0].Ref)

	start := streamedText(t, e.say(t, "s", "да"))
	assert.Contains(t, start, "Започнах")
	assert.Contains(t, start, "всичко в един файл")
	running, err := e.plans.Running(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, running)
	assert.Equal(t, taskrun.KindResearch, running.Kind)
	assert.Equal(t, 1, e.waker.n)
}

func TestWholeToAResearchIsNoTaskOverLinksOfTheLibrary(t *testing.T) {
	e, _, _, _ := researchEnv(t, ocrResults)
	e.say(t, "s", researchAsk)
	e.say(t, "s", researchFollowUp)

	e.say(t, "s", "цялата")

	running, err := e.plans.Running(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Nil(t, running, "the pages of a search are not put into the library to be read whole")
}

func TestAResearchWithNoReadablePageSaysSo(t *testing.T) {
	e, _, _, _ := researchEnv(t, `{"results":[{"title":"OCR","url":"https://www.youtube.com/watch?v=x"}]}`)
	out := streamedText(t, e.say(t, "s", "намери в интернет статии за OCR и направи справочник във файл"))
	assert.Contains(t, out, "не намерих страници, които мога да прочета като текст")
	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Nil(t, p)
}

func TestAResearchIsLeftToTheModelWhenTheRequestCannotBeRead(t *testing.T) {
	e, _, searches, reader := researchEnv(t, ocrResults)
	reader.fail = true
	asked := len(e.model.Requests)
	e.say(t, "s", "намери в интернет статии за OCR и направи справочник във файл")
	assert.Zero(t, *searches, "nothing is searched")
	assert.Greater(t, len(e.model.Requests), asked, "the model answers the turn")
	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Nil(t, p)
}

func TestReadPageReadsHereThenSaysWhyNot(t *testing.T) {
	pages := &fakePages{title: "Tesseract", text: "Tesseract reads text."}
	r := &DefaultToolRegistry{}
	r.WithLocalWebFetch(pages)
	title, text, err := r.ReadPage(context.Background(), "https://a.example/x")
	require.NoError(t, err)
	assert.Equal(t, "Tesseract", title)
	assert.Equal(t, "Tesseract reads text.", text)

	pages.err = &webfetch.Error{Reason: webfetch.ReasonBlocked}
	_, _, err = r.ReadPage(context.Background(), "https://a.example/x")
	require.Error(t, err)
	assert.Equal(t, "сайтът не пуска програми (отказ 401/403)", err.Error())

	_, _, err = r.ReadPage(context.Background(), "http://localhost/x")
	require.Error(t, err)
	assert.Equal(t, 1+1, pages.calls, "a local address is not fetched")

	_, _, err = (&DefaultToolRegistry{}).ReadPage(context.Background(), "https://a.example/x")
	assert.EqualError(t, err, "четенето на страници е изключено")
}
