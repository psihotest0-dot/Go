package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/webfetch"
)

func TestAsksAboutLinks(t *testing.T) {
	for _, m := range []string{
		// The Flet chat of 2026-11-20, word for word.
		"пробвай да достигнеш до тях първо чрез търсене и след това извличане ,може първо за всеки линк да ми обясниш дали е цяла книга или статия и колко място ще заеме",
		"какво са тези линкове?",
		"за всеки линк ми кажи какво е и колко е голям",
		"колко страници са тези източници",
		"what are these links and how big are they?",
	} {
		assert.True(t, asksAboutLinks(m), m)
	}
	for _, m := range []string{
		"добави ги в библиотеката",
		"за всеки линк създай файл с информация",
		"добави в библиотеката линковете, които са PDF",            // a task with the words of the question
		"за всеки линк направи файл с ключовите факти на статията", // the same
		"запиши тези линкове",
		"колко е часът?",                                                      // no link
		"какво е стоицизмът според книгите",                                   // no link
		"ето източниците: https://a.example/paper.pdf https://b.example/book", // the words of a link ask nothing
		"",
	} {
		assert.False(t, asksAboutLinks(m), m)
	}
}

func TestTheLinksAreTheOnesOfTheNewestMessageThatHasAny(t *testing.T) {
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "старо: https://old.example/a.pdf"},
		{Role: openai.ChatMessageRoleUser, Content: "ето ги: https://a.example/x.pdf, https://b.example/y и пак https://a.example/x.pdf."},
		{Role: openai.ChatMessageRoleAssistant, Content: "виж https://assistant.example/z"},
		{Role: openai.ChatMessageRoleUser, Content: "какво са тези линкове?"},
	}
	links, more := lastUserLinks(history)
	assert.Equal(t, []string{"https://a.example/x.pdf", "https://b.example/y"}, links, "once each, in order; not the older message nor the assistant's")
	assert.False(t, more)

	var long strings.Builder
	for i := range maxProbeLinks + 3 {
		long.WriteString(" https://many.example/p" + strings.Repeat("x", i+1))
	}
	links, more = lastUserLinks([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: long.String()}})
	assert.Len(t, links, maxProbeLinks)
	assert.True(t, more)

	links, _ = lastUserLinks([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "няма линкове"}})
	assert.Empty(t, links)
}

func htmlDoc(title, text string) *webfetch.Document {
	page := "<html><head><title>" + title + "</title></head><body><p>" + text + "</p></body></html>"
	return &webfetch.Document{Name: title + ".html", Title: title, Kind: webfetch.KindHTML, Body: strings.NewReader(page)}
}

func TestEachLinkIsOpenedAndSaidWhatItIs(t *testing.T) {
	pdfBody := []byte("%PDF-1.4 " + strings.Repeat("x", 3000))
	f := &fakeFetcher{
		docs: map[string]*webfetch.Document{
			"https://a.example/paper.pdf": {Name: "paper.pdf", Kind: webfetch.KindPDF, Body: bytes.NewReader(pdfBody)},
			"https://b.example/article":   htmlDoc("A Tug of War", strings.Repeat("word ", 7000)),
		},
		errs: map[string]error{
			"https://c.example/abstract": &webfetch.Error{Reason: webfetch.ReasonAbstractOnly},
			"https://d.example/blocked":  &webfetch.Error{Reason: webfetch.ReasonBlocked, Detail: "403"},
			"https://e.example/huge.pdf": webfetch.ErrTooLarge,
		},
	}
	r := addTool(f, &fakeAdder{})
	links := []string{"https://a.example/paper.pdf", "https://b.example/article", "https://c.example/abstract", "https://d.example/blocked", "https://e.example/huge.pdf"}

	probes := r.probeLinks(context.Background(), uuid.New(), links)

	require.Len(t, probes, 5)
	assert.Equal(t, webfetch.KindPDF, probes[0].Kind)
	assert.Equal(t, int64(len(pdfBody)), probes[0].Bytes, "the PDF is read to its end")
	assert.Equal(t, webfetch.KindHTML, probes[1].Kind)
	assert.InDelta(t, 7000, probes[1].Words, 10, "the words of the text of the page")
	assert.Equal(t, string(webfetch.ReasonAbstractOnly), probes[2].Reason)
	assert.Equal(t, string(webfetch.ReasonBlocked), probes[3].Reason)
	assert.Equal(t, string(webfetch.ReasonTooLarge), probes[4].Reason)
	for i, p := range probes {
		assert.Equal(t, links[i], p.Link, "in the order given")
	}

	text := linkProbeText(probes, false, 50<<20)
	assert.Contains(t, text, "нищо не е записано")
	assert.Contains(t, text, "1. paper.pdf — PDF, 3 KB")
	assert.Contains(t, text, "2. A Tug of War — статия (уеб страница), ≈ 7 000 думи", "in hundreds (the title has 4 more)")
	assert.Contains(t, text, "само резюме")
	assert.Contains(t, text, "не пуска програми")
	assert.Contains(t, text, "по-голям от 50,0 MB")
	assert.Contains(t, text, "Могат да се добавят в библиотеката: 2")
}

func TestALinkWhoseDocumentIsInTheLibraryIsSaidToBeThere(t *testing.T) {
	e := newToolEnv(t)
	body := []byte("%PDF-1.4 the same paper")
	sum := sha256.Sum256(body)
	_, err := e.db.Queries.CreateIngestedFile(context.Background(), db.CreateIngestedFileParams{
		UserID: e.userID, FileName: "Lou_Polk_Skouras.pdf", FileHash: hex.EncodeToString(sum[:]),
		MimeType: "application/pdf", FileSize: int64(len(body)), Status: "completed", LocalPath: "x",
	})
	require.NoError(t, err)
	f := &fakeFetcher{docs: map[string]*webfetch.Document{
		"https://a.example/paper.pdf": {Name: "paper.pdf", Kind: webfetch.KindPDF, Body: bytes.NewReader(body)},
	}}
	e.reg.WithLibraryAdd(f, &fakeAdder{})

	probes := e.reg.probeLinks(context.Background(), e.userID, []string{"https://a.example/paper.pdf"})
	require.Len(t, probes, 1)
	assert.Equal(t, "Lou_Polk_Skouras.pdf", probes[0].InLibrary)
	text := linkProbeText(probes, false, 50<<20)
	assert.Contains(t, text, "вече е в библиотеката като „Lou_Polk_Skouras.pdf“")
	assert.Contains(t, text, "Нищо ново за добавяне")

	other := e.reg.probeLinks(context.Background(), uuid.New(), []string{"https://a.example/paper.pdf"})
	assert.Empty(t, other[0].InLibrary, "another user's library is not looked into")
}

func TestASlowSiteTakesNoLongerThanItsTime(t *testing.T) {
	f := &fakeFetcher{hold: time.Hour}
	r := addTool(f, &fakeAdder{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	probes := r.probeLinks(ctx, uuid.New(), []string{"https://slow.example/a"})
	assert.Equal(t, string(webfetch.ReasonTimeout), probes[0].Reason)
}

// The whole turn: the code answers, the model is not asked.
func TestTheQuestionAboutTheLinksIsAnsweredByTheCode(t *testing.T) {
	e := newAttachmentEnv(t)
	f := &fakeFetcher{docs: map[string]*webfetch.Document{
		"https://a.example/paper.pdf": {Name: "paper.pdf", Kind: webfetch.KindPDF, Body: bytes.NewReader([]byte("%PDF-1.4 paper"))},
	}}
	e.reg.WithLibraryAdd(f, &fakeAdder{})
	model := answeringModel()
	o := orchestratorWith(t, e, model).WithLibraryAdd()

	askAboutFilesWith(t, o, e.userID, "links", "ето източниците: https://a.example/paper.pdf")
	asked := len(model.Requests)
	out := streamedText(t, askAboutFilesWith(t, o, e.userID, "links", "за всеки линк ми кажи какво е и колко е голям"))

	assert.Equal(t, asked, len(model.Requests), "the model is not asked")
	assert.Contains(t, out, "1. paper.pdf — PDF")
	assert.Equal(t, []string{"https://a.example/paper.pdf"}, f.asked, "the link was opened once")

	// Without add_to_library the code does not open links.
	off := orchestratorWith(t, e, answeringModel())
	f.asked = nil
	askAboutFilesWith(t, off, e.userID, "links-off", "ето: https://a.example/paper.pdf")
	askAboutFilesWith(t, off, e.userID, "links-off", "за всеки линк ми кажи какво е и колко е голям")
	assert.Empty(t, f.asked)
}

func TestWithoutLinksTheCodeSaysWhereToPutThem(t *testing.T) {
	e := newAttachmentEnv(t)
	e.reg.WithLibraryAdd(&fakeFetcher{}, &fakeAdder{})
	model := answeringModel()
	o := orchestratorWith(t, e, model).WithLibraryAdd()

	out := streamedText(t, askAboutFilesWith(t, o, e.userID, "none", "може първо за всеки линк да ми обясниш дали е книга или статия"))
	assert.Contains(t, out, "Не виждам линкове")
	assert.Empty(t, model.Requests)
}

func TestTheHelpListNamesTheLinkCheck(t *testing.T) {
	e := newAttachmentEnv(t)
	assert.Contains(t, orchestratorWith(t, e, answeringModel()).WithLibraryAdd().helpText(), "За всеки линк ми кажи какво е")
	assert.NotContains(t, orchestratorWith(t, e, answeringModel()).helpText(), "За всеки линк ми кажи какво е")
}

// askAboutFilesWith sends one message of the person in a conversation, as Flet does.
func askAboutFilesWith(t *testing.T, o *Orchestrator, userID uuid.UUID, session, message string) string {
	t.Helper()
	body, _ := json.Marshal(ChatRequest{SessionID: session, Content: message})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

func TestACopyOfTheWaybackMachineIsSaidToBeOne(t *testing.T) {
	p := linkProbe{Link: "https://alpha.example/a", Title: "Trading Costs", Kind: webfetch.KindHTML, Words: 3128,
		Archived: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC), Because: string(webfetch.ReasonBlocked)}
	line := probeLine(p, 50<<20)
	assert.Equal(t, "статия (уеб страница), ≈ 3 100 думи; сайтът не пуска програми, затова прочетох копието от 16.07.2026 в Wayback Machine", line)
}

func TestSizesAndCountsAreSaidTheWayAPersonReadsThem(t *testing.T) {
	assert.Equal(t, "1 KB", sizeWords(10))
	assert.Equal(t, "600 KB", sizeWords(600<<10))
	assert.Equal(t, "8,5 MB", sizeWords(8912896))
	assert.Equal(t, "53 700", thousands(roundWords(53731)))
	assert.Equal(t, "640", thousands(roundWords(640)))
	assert.Equal(t, "1 200 000", thousands(1200000))
}
