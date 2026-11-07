package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

func passage(name, text string) api.SourceInfo {
	return api.SourceInfo{ChunkID: uuid.New(), FileName: name, ContentSnippet: text, SourceType: "text"}
}

// ---------------------------------------------------------------- the budget
// The panel lists what the model was given. A memory or summary that the prompt
// budget cut off was never read, and must not be shown as a source.
func TestLoadContextListsOnlyTheSourcesTheBudgetLeftInThePrompt(t *testing.T) {
	e := newContextEnv(t)
	for i := 0; i < topMemoryLimit; i++ {
		e.memory(t, fmt.Sprintf("profile filler %02d", i), 9, 0.90)
	}
	for i, distance := range []float64{0.10, 0.11, 0.12} {
		e.memory(t, fmt.Sprintf("near memory %d %s", i, strings.Repeat("x", 120)), 1, distance)
	}
	e.summaries(t, map[string]float64{"Close summary": 0.12, "Closer summary": 0.10})

	everything, _ := e.load(t)
	require.Len(t, everything.Memories, 3, "with the default budget everything fits")
	require.Len(t, everything.Chunks, 2)
	require.Len(t, everything.Sources, 5)

	// Room for the profile and exactly one retrieved memory (they cost the same).
	room := estimateTokens(everything.Memories[0]) + 1
	for _, m := range everything.TopMemories {
		room += estimateTokens(m)
	}
	small := legacyBudgets()
	small.promptContextTokens = room
	e.o.budgets = small

	cut, _ := e.load(t)

	require.Len(t, cut.Memories, 1, "the budget cut two memories")
	assert.Empty(t, cut.Chunks, "and both summaries")
	require.Len(t, cut.Sources, 1, "the panel lists the one memory the model has")
	assert.Equal(t, "memory", cut.Sources[0].SourceType)
	assert.Equal(t, cut.Memories[0], cut.Sources[0].ContentSnippet)
}

// ------------------------------------------------------------- the turn state
func TestTheTurnAnnouncesEachFoundPassageOnce(t *testing.T) {
	_, turn := withTurn(context.Background(), "s", false)
	first, second, third := passage("a.txt", "one"), passage("b.txt", "two"), passage("c.txt", "three")

	turn.addSources([]api.SourceInfo{first, second})
	assert.Equal(t, []api.SourceInfo{first, second}, turn.newSources())
	assert.Empty(t, turn.newSources(), "already announced")

	turn.addSources([]api.SourceInfo{second, third})
	assert.Equal(t, []api.SourceInfo{third}, turn.newSources(), "a passage found twice is one source")
	assert.Equal(t, []api.SourceInfo{first, second, third}, turn.foundSources(), "all of them, in the order found")
}

func TestOutsideAChatTurnThereAreNoFoundSources(t *testing.T) {
	var turn *turnState // what turnFrom gives to a context that has no turn

	turn.addSources([]api.SourceInfo{passage("a.txt", "one")})

	assert.Empty(t, turn.newSources())
	assert.Empty(t, turn.foundSources())
}

func TestTheStoredAnswerKeepsThePromptSourcesAndAddsTheFoundPassages(t *testing.T) {
	_, turn := withTurn(context.Background(), "s", false)
	inPrompt, found := passage("summary.txt", "in the prompt"), passage("book.txt", "found by the tool")
	turn.addSources([]api.SourceInfo{inPrompt, found})

	got := answerSources(turn, []api.SourceInfo{inPrompt})

	assert.Equal(t, []api.SourceInfo{inPrompt, found}, got)
	assert.Equal(t, []api.SourceInfo{inPrompt}, answerSources(nil, []api.SourceInfo{inPrompt}), "no turn, nothing to add")
}

func TestAPassageIsShownAsTextOrOCRWithoutAScore(t *testing.T) {
	id := uuid.New()

	plain := passageSource(retrieval.SearchResult{ID: id, FileName: "book.pdf", Content: "  обикновен текст  ", Score: 0.031})
	assert.Equal(t, api.SourceInfo{ChunkID: id, FileName: "book.pdf", ContentSnippet: "обикновен текст", SourceType: "text"}, plain,
		"the hybrid search ranks the passages but measures none of them")

	scanned := passageSource(retrieval.SearchResult{ID: id, FileName: "scan.pdf", Content: "ред едно\n" + api.OCRMarker + "\nред две"})
	assert.Equal(t, "ocr", scanned.SourceType)
	assert.NotContains(t, scanned.ContentSnippet, "[source:", "the marker is the pipeline's, not the book's")
}

// -------------------------------------------------------------- the SSE frames
// passageFinder is a tool registry whose tools find the given passages.
type passageFinder struct{ found []api.SourceInfo }

func (f *passageFinder) ExecuteTool(ctx context.Context, _ string, _ map[string]interface{}) (interface{}, error) {
	turnFrom(ctx).addSources(f.found)
	return map[string]interface{}{"status": "success"}, nil
}

// streamedFrames runs one streamed turn and returns the body split into SSE frames.
func streamedFrames(t *testing.T, mock *llm.MockLLM, tools ToolRegistry, promptSources []api.SourceInfo) []string {
	t.Helper()
	o := newGenerateOrchestrator(mock, tools)
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", nil)
	ctx, _ := withTurn(req.Context(), "s", false)
	rec := httptest.NewRecorder()

	_, _, err := o.streamResponse(rec, req.WithContext(ctx), "s",
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "какво пише в книгата"}},
		toolDefinitions(), promptSources)

	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
}

// sourceFrames are the decoded "sources" frames and the index of each in frames.
func sourceFrames(t *testing.T, frames []string) (decoded [][]api.SourceInfo, at []int) {
	t.Helper()
	for i, frame := range frames {
		data, ok := strings.CutPrefix(frame, "event: sources\ndata: ")
		if !ok {
			continue
		}
		var sources []api.SourceInfo
		require.NoError(t, json.Unmarshal([]byte(data), &sources))
		decoded = append(decoded, sources)
		at = append(at, i)
	}
	return decoded, at
}

func firstContentFrame(frames []string) int {
	for i, frame := range frames {
		if strings.HasPrefix(frame, `data: {"content"`) {
			return i
		}
	}
	return -1
}

func toolThenAnswer(answer ...openai.ChatCompletionStreamResponse) *llm.MockLLM {
	return &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_0", "search_documents", `{"query":"книгата"}`)},
		answer,
	}}
}

func TestPassagesFoundByATool_ReachThePanelInASecondSourcesFrameBeforeTheText(t *testing.T) {
	inPrompt := passage("summary.txt", "in the prompt")
	one, two := passage("book.txt", "first passage"), passage("book.txt", "second passage")
	tools := &passageFinder{found: []api.SourceInfo{inPrompt, one, two}}

	frames := streamedFrames(t, toolThenAnswer(textChunk("Отговор")), tools, []api.SourceInfo{inPrompt})

	decoded, at := sourceFrames(t, frames)
	require.Len(t, decoded, 2, "the prompt's sources, then the tool's")
	assert.Equal(t, []api.SourceInfo{inPrompt}, decoded[0])
	assert.Equal(t, []api.SourceInfo{one, two}, decoded[1], "only what the client does not have yet")
	assert.Less(t, at[1], firstContentFrame(frames), "the panel fills as the answer starts")
	assert.Equal(t, "data: [DONE]", frames[len(frames)-1])
}

func TestPassagesFoundByAToolAreSentEvenWhenNoTextFollowsTheLastToolCall(t *testing.T) {
	found := passage("book.txt", "a passage")
	tools := &passageFinder{found: []api.SourceInfo{found}}
	stop := openai.ChatCompletionStreamResponse{Choices: []openai.ChatCompletionStreamChoice{{FinishReason: openai.FinishReasonStop}}}

	frames := streamedFrames(t, toolThenAnswer(stop), tools, nil)

	decoded, _ := sourceFrames(t, frames)
	require.Len(t, decoded, 2)
	assert.Equal(t, []api.SourceInfo{found}, decoded[1])
}

func TestNoSecondSourcesFrameWhenTheToolsFindNothingNew(t *testing.T) {
	inPrompt := passage("summary.txt", "in the prompt")

	for name, tools := range map[string]ToolRegistry{
		"nothing found":      &passageFinder{},
		"only what was sent": &passageFinder{found: []api.SourceInfo{inPrompt}},
	} {
		t.Run(name, func(t *testing.T) {
			frames := streamedFrames(t, toolThenAnswer(textChunk("Отговор")), tools, []api.SourceInfo{inPrompt})

			decoded, _ := sourceFrames(t, frames)
			assert.Len(t, decoded, 1)
		})
	}
}

func TestAnAnswerWithoutToolsHasTheOneSourcesFrame(t *testing.T) {
	mock := &llm.MockLLM{StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Отговор")}}

	frames := streamedFrames(t, mock, &passageFinder{}, nil)

	decoded, at := sourceFrames(t, frames)
	require.Len(t, decoded, 1)
	assert.Equal(t, 0, at[0], "first, as the contract says")
	assert.Empty(t, decoded[0])
}

// ------------------------------------------------------------ the real search
// What search_documents returns to the model is what the panel lists.
func TestSearchDocumentsPutsThePassagesItReturnedIntoTheTurnsSources(t *testing.T) {
	e := newToolEnv(t)
	ctx := context.Background()
	const question = "Колко е дълъг Дунав според книгата"

	file, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: e.userID, FileName: "reki.txt", FileHash: "hash-" + uuid.NewString(),
		MimeType: "text/plain", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)
	vectors, err := (&llm.MockEmbedder{}).EmbedTexts(ctx, []string{question}, true)
	require.NoError(t, err)
	texts := []string{question, "Страница сканирана с OCR.\n" + api.OCRMarker + "\n" + question}
	var arg db.CreateDocumentChunksBatchParams
	arg.EmbeddingModel = config.DefaultEmbedModel
	for i, text := range texts {
		arg.FileIds = append(arg.FileIds, file.ID)
		arg.UserIds = append(arg.UserIds, e.userID)
		arg.ChunkIndices = append(arg.ChunkIndices, int32(i))
		arg.Texts = append(arg.Texts, text)
		arg.Embeddings = append(arg.Embeddings, pgvector.NewVector(vectors[0]))
		arg.ChunkTypes = append(arg.ChunkTypes, "raw_text")
		arg.Metadatas = append(arg.Metadatas, "{}")
	}
	require.NoError(t, e.db.Queries.CreateDocumentChunksBatch(ctx, arg))

	turnCtx, turn := e.turn("s", false)
	res, err := e.reg.ExecuteTool(turnCtx, "search_documents", map[string]interface{}{"query": question})
	require.NoError(t, err)
	returned := resultMap(t, res)["results"].([]map[string]interface{})
	require.Len(t, returned, 2, "the model got both passages")

	found := turn.foundSources()
	require.Len(t, found, 2, "and so does the panel")
	byType := map[string]api.SourceInfo{}
	for _, s := range found {
		assert.Equal(t, "reki.txt", s.FileName)
		assert.Zero(t, s.Score)
		assert.Empty(t, s.ScoreType)
		byType[s.SourceType] = s
	}
	assert.Equal(t, question, byType["text"].ContentSnippet)
	assert.NotContains(t, byType["ocr"].ContentSnippet, "[source:")
	for _, r := range returned {
		assert.Contains(t, []uuid.UUID{found[0].ChunkID, found[1].ChunkID}, r["id"], "the same chunks the model read")
	}
}
