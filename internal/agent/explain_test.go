package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	pgvector "github.com/pgvector/pgvector-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func TestExplainSubject(t *testing.T) {
	// The questions of the measurement (explain_2026-11-22), with the subject the templates use.
	for message, subject := range map[string]string{
		"не разбирам какво са горутините в Go":                                "горутините в Go",
		"обясни ми каналите в Go, не ги разбирам":                             "каналите в Go",
		"какво е Docker контейнер и с какво се различава от виртуална машина": "Docker контейнер",
		"не разбирам какво прави Kubernetes":                                  "Kubernetes",
		"обясни ми какво е указател в C++":                                    "указател в C++",
		"какво е индекс в базата данни и защо ускорява заявките":              "индекс в базата данни",
		"Какво е хеш функция?":                                                "хеш функция",
		"какво значи разделяне на задачите, за което пише в Смелостта":        "разделяне на задачите",
		"а как работи диверсификацията на портфейла?":                         "диверсификацията на портфейла",
		"What is a hash function?": "hash function",
		"explain channels in Go":   "channels in Go",
	} {
		assert.Equal(t, subject, explainSubject(message), message)
	}

	for _, message := range []string{
		// The owner's real messages with "разбира" in them (2026-11-22, 0 of his 121 are caught):
		// about Lili, not about a subject.
		"Не.различно е .не го разбираш за момента ,ще трябва малко корекция",
		"Това много не го разбирам да ти кажа честно",
		"мислех че се подразбираше да започнеш..,започни търсенето",
		// A follow-up is not a new subject: today's search handles it (earlier_answer.go).
		"обясни ми го по-просто",
		"обясни ми пак това",
		"explain it again please",
		// About the two who talk, the news, too short, not a request for an explanation.
		"какво е времето днес",
		"какво правиш тази вечер",
		"Какво ново по света днес",
		"обясни ми",
		"какво е",
		"Какво казва Марк Аврелий за смъртта?",
		"дай ми рецепта за палачинки",
		"обясни ми какво казват стоиците за нещата, които зависят от нас",
		"какво е " + strings.Repeat("много ", 70),
	} {
		assert.Empty(t, explainSubject(message), message)
	}
}

func TestExplainQueries(t *testing.T) {
	assert.Equal(t, []string{
		"горутини Go", "what are goroutines in Go",
		"горутините в Go е", "горутините в Go пример", "защо горутините в Go", "как работи горутините в Go",
		"what is goroutines in Go", "goroutines in Go example", "how does goroutines in Go work",
	}, explainQueries("горутини Go", "what are goroutines in Go", "горутините в Go", true))

	// The subject of a Bulgarian message may be a Latin word: the Bulgarian templates still go.
	q := explainQueries("Kubernetes", "what does Kubernetes do", "Kubernetes", true)
	assert.Contains(t, q, "как работи Kubernetes")
	assert.Contains(t, q, "how does Kubernetes work", "the English subject without its \"do\"")
	assert.Contains(t, explainQueries("Docker", "what is a Docker container and how is it different from a virtual machine", "Docker контейнер", true),
		"Docker container example", "the English subject is cut where the second question starts")

	// Without the English query: the Bulgarian templates only.
	assert.Equal(t, []string{"хеш функция", "хеш функция е", "хеш функция пример", "защо хеш функция", "как работи хеш функция"},
		explainQueries("хеш функция", "", "хеш функция", true), "the same query is searched once")

	// An English message: the English templates of its own subject.
	assert.Equal(t, []string{"hash function", "what is hash function", "hash function example", "how does hash function work"},
		explainQueries("hash function", "", "hash function", false))
}

func TestPickDossierKeepsTwoPassagesOfABookAndEightInAll(t *testing.T) {
	var merged []retrieval.SearchResult
	for i := range 6 {
		merged = append(merged, retrieval.SearchResult{FileName: "go.pdf", Score: 0.9 - float64(i)/100})
	}
	for _, f := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		merged = append(merged, retrieval.SearchResult{FileName: f, Score: 0.5})
	}
	picked := pickDossier(merged)
	require.Len(t, picked, explainPassages)
	assert.Equal(t, "go.pdf", picked[0].FileName)
	assert.Equal(t, "go.pdf", picked[1].FileName)
	assert.Equal(t, "a", picked[2].FileName, "the third passage of a book gives its place to the next book")
	assert.Empty(t, pickDossier(nil))
}

func TestTheExplanationIsOffUnlessTurnedOn(t *testing.T) {
	const message = "не разбирам какво са горутините в Go"
	o := &Orchestrator{}
	assert.Empty(t, o.explainSubjectFor(message))
	assert.False(t, o.planSignals(context.Background(), message, false, false).ForcedTool)

	o.WithExplainDossier(false)
	assert.Equal(t, "горутините в Go", o.explainSubjectFor(message+"\n\n"+closingNoteQuestion), "the closing note is not part of the subject")
	assert.True(t, o.planSignals(context.Background(), message, false, false).ForcedTool,
		"an explanation gets no plan of an answer")
}

func TestAnExplanationSearchesTheLibraryFirst(t *testing.T) {
	// "не разбирам ..." does not open with a question word: a question to the books it is not.
	const message = "не разбирам какво са горутините в Go"
	require.Empty(t, forcedFirstTool(toolDefinitions(), message))

	run := func(explain bool) *llm.MockLLM {
		mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
			{toolCallChunk(0, "call_0", searchDocumentsTool, `{"query":"горутини"}`)},
			{textChunk("Горутината е...")},
		}}
		o := newGenerateOrchestrator(mock, &fakeToolRegistry{result: map[string]interface{}{"status": "success"}})
		ctx, turn := withTurn(context.Background(), "s", false)
		if explain {
			turn.setExplain("горутините в Go")
		}
		_, _, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: message}}, toolDefinitions(), nil)
		require.NoError(t, err)
		return mock
	}

	first := run(true).Requests[0]
	require.Len(t, first.Tools, 1)
	assert.Equal(t, searchDocumentsTool, first.Tools[0].Function.Name)
	assert.Equal(t, "required", first.ToolChoice)

	assert.Nil(t, run(false).Requests[0].ToolChoice, "without the dossier the model chooses, as before")
}

func TestAnExplanationGoesWhereExplainLLMSays(t *testing.T) {
	// A question to the books, which LIBRARY_LLM sends to the cloud.
	const message = "обясни ми каналите в Go"
	explain := func(t *testing.T, o *Orchestrator, ctx context.Context) (string, string) {
		t.Helper()
		ctx, turn := withTurn(ctx, "s", false)
		turn.setExplain("каналите в Go")
		content, _, err := o.generateResponse(ctx, []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: message}}, nil, nil)
		require.NoError(t, err)
		return content, turnFrom(ctx).answeredByWhom().Provider
	}

	t.Run("local by default", func(t *testing.T) {
		before := routeCount("local", "explain_local")
		content, by := explain(t, routed(say("локално"), say("облак"), true).WithExplainDossier(false), context.Background())
		assert.Equal(t, "локално", content)
		assert.Equal(t, "local", by)
		assert.EqualValues(t, 1, routeCount("local", "explain_local")-before)
	})
	t.Run("the cloud with EXPLAIN_LLM=deepseek", func(t *testing.T) {
		before := routeCount("paid", "explain")
		content, by := explain(t, routed(say("локално"), say("облак"), false).WithExplainDossier(true), context.Background())
		assert.Equal(t, "облак", content, "even with LIBRARY_LLM=local")
		assert.Equal(t, "deepseek", by)
		assert.EqualValues(t, 1, routeCount("paid", "explain")-before)
	})
	t.Run("the button of the conversation decides first", func(t *testing.T) {
		content, _ := explain(t, routed(say("локално"), say("облак"), true).WithExplainDossier(true), WithLLMChoice(context.Background(), no()))
		assert.Equal(t, "локално", content)
		content, _ = explain(t, routed(say("локално"), say("облак"), true).WithExplainDossier(false), WithLLMChoice(context.Background(), yes()))
		assert.Equal(t, "облак", content)
	})
}

func TestSearchDocumentsGivesTheDossierOfAnExplanation(t *testing.T) {
	e := newToolEnv(t)
	log := &embedderLog{}
	reg := NewDefaultToolRegistry(retrieval.NewRetrievalService(e.db, log), e.db, e.svc)
	ctx := context.Background()

	// A book of five passages about goroutines and one of two.
	books := map[string][]string{
		"go.pdf": {
			"Go programs run many functions at the same time.",
			"A goroutine is a lightweight thread managed by the Go runtime.",
			"You start a goroutine with the go keyword before a call.",
			"Goroutines talk to each other through channels.",
			"The scheduler of Go puts goroutines on threads of the system.",
		},
		"concurrency.txt": {
			"Concurrency is about dealing with many things at once.",
			"A goroutine costs a few kilobytes of stack.",
		},
	}
	for name, texts := range books {
		file, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
			UserID: e.userID, FileName: name, FileHash: "hash-" + name,
			MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
		})
		require.NoError(t, err)
		vectors, err := (&llm.MockEmbedder{}).EmbedTexts(ctx, texts, false)
		require.NoError(t, err)
		var arg db.CreateDocumentChunksBatchParams
		arg.EmbeddingModel = config.DefaultEmbedModel
		for i, text := range texts {
			arg.FileIds = append(arg.FileIds, file.ID)
			arg.UserIds = append(arg.UserIds, e.userID)
			arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
			arg.Texts = append(arg.Texts, text)
			arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(vectors[i]))
			arg.ChunkTypes = append(arg.ChunkTypes, "raw_text")
			arg.Metadatas = append(arg.Metadatas, "{}")
		}
		require.NoError(t, e.db.Queries.CreateDocumentChunksBatch(ctx, arg))
	}

	turnCtx, turn := e.turn("s", false)
	turnCtx = WithCurrentQuery(turnCtx, "не разбирам какво са горутините в Go")
	turn.setExplain("горутините в Go")
	args := map[string]interface{}{"query": "горутини в Go", "query_en": "what are goroutines in Go"}

	res, err := reg.ExecuteTool(turnCtx, "search_documents", args)
	require.NoError(t, err)
	out := resultMap(t, res)
	assert.Equal(t, "success", out["status"])
	note, _ := out["note"].(string)
	assert.Contains(t, note, `These passages come from 2 books of the user: `, "the recipe is the note of the result")
	assert.Contains(t, note, `"go"`, "it names the books of the dossier")
	assert.Contains(t, note, `"concurrency"`)
	assert.Contains(t, note, "Use at least two of these books")
	assert.ElementsMatch(t, explainQueries("горутини в Go", "what are goroutines in Go", "горутините в Go", true), log.seen(),
		"the two queries of the model and the templates, each searched once")

	results := out["results"].([]map[string]interface{})
	require.NotEmpty(t, results)
	perFile := map[string]int{}
	for i, r := range results {
		assert.Equal(t, i+1, r["n"], "the passages are numbered")
		assert.NotContains(t, r, "id")
		perFile[r["file_name"].(string)]++
	}
	assert.LessOrEqual(t, perFile["go.pdf"], explainPerFile, "at most two passages of a book")

	// Every passage of these books has a neighbour: the first three bring the text around them.
	contains := func(content string) int {
		n := 0
		for _, texts := range books {
			for _, text := range texts {
				if strings.Contains(content, text) {
					n++
				}
			}
		}
		return n
	}
	for i, r := range results {
		if i < explainWithNeighbours {
			assert.GreaterOrEqual(t, contains(r["content"].(string)), 2, "passage %d brings its neighbours", i+1)
		} else {
			assert.Equal(t, 1, contains(r["content"].(string)), "passage %d comes alone", i+1)
		}
	}
	assert.Len(t, turn.foundSources(), len(results), "the sources panel lists the passages of the dossier")

	// The second search of the turn is an ordinary one.
	res, err = reg.ExecuteTool(turnCtx, "search_documents", args)
	require.NoError(t, err)
	out = resultMap(t, res)
	assert.NotContains(t, out, "note")
	assert.Contains(t, out["results"].([]map[string]interface{})[0], "id")
}

func TestPrepareTurnMarksAnExplanation(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	const message = "не разбирам какво са горутините в Go"
	storeReplies(t, e, "ex-1", 0, message)

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "ex-1", Content: message}, false)
	require.NoError(t, err)
	assert.False(t, prep.turn.explaining(), "off: an ordinary turn")

	o.WithExplainDossier(false)
	prep, err = o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "ex-1", Content: message}, false)
	require.NoError(t, err)
	assert.True(t, prep.turn.explaining())
	assert.Equal(t, "горутините в Go", prep.turn.takeExplain())

	storeReplies(t, e, "ex-2", 0, "здравей, как си")
	prep, err = o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "ex-2", Content: "здравей, как си"}, false)
	require.NoError(t, err)
	assert.False(t, prep.turn.explaining(), "not every turn is an explanation")
}

func TestABookIsNamedByItsTitleNotItsFileName(t *testing.T) {
	for file, want := range map[string]string{
		"books.site_some-book-about-go.pdf":              "some book about go",
		"Author-Name_-_Zaglavie_na_knigata_-_1234-b.pdf": "Author Name Zaglavie na knigata",
		"Заглавие - Автор - books.me.pdf":                "Заглавие - Автор",
		"A Book of C++ 3rd Edition.epub":                 "A Book of C++ 3rd Edition",
		"why-something.pdf":                              "why something", "Node.js-Design.pdf": "Node.js Design", "Some C++ Cookbook - Publisher (2020).pdf": "Some C++ Cookbook - Publisher",
		"the bookof Things.epub": "the bookof Things",
		"книга.pdf":              "книга",
		".pdf":                   ".pdf",
	} {
		assert.Equal(t, want, bookTitle(file), file)
	}
	assert.Equal(t, 90, utf8.RuneCountInString(bookTitle(strings.Repeat("дума ", 40)+".pdf")), "a long title is cut")
}

// The recipe names each book of the dossier once and asks for two of them; with one book it
// only asks for its name.
func TestTheRecipeNamesTheBooksOfTheDossier(t *testing.T) {
	picked := []retrieval.SearchResult{{FileName: "books.site_some-book-about-go.pdf"}, {FileName: "another-go-book.pdf"}, {FileName: "books.site_some-book-about-go.pdf"}}
	recipe := explainRecipe(true, picked)
	assert.Contains(t, recipe, `These passages come from 2 books of the user: "some book about go", "another go book". `)
	assert.Contains(t, recipe, "in Bulgarian:")
	assert.Contains(t, recipe, "Use at least two of these books, and for each thing you take say which book it is from, by its title.")
	assert.Contains(t, recipe, "Take the facts only from the passages.")

	one := explainRecipe(false, picked[:1])
	assert.Contains(t, one, `These passages come from 1 books of the user: "some book about go". `)
	assert.Contains(t, one, "in the language of the person's message:")
	assert.NotContains(t, one, "at least two")
	assert.Contains(t, one, "Say which book it is from, by its title.")
}
