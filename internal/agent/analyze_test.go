package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pgvector/pgvector-go"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
)

func TestLanguageOf(t *testing.T) {
	for name, want := range map[string]string{
		"main.py": "Python", "app/server.go": "Go", "index.ts": "JavaScript", "lib.rs": "Rust",
		"Dockerfile": "Docker", "docker-compose.yml": "Docker", "query.sql": "SQL", "notes.md": "", "article.txt": "",
	} {
		got, _ := languageOf(name)
		assert.Equal(t, want, got, name)
	}
	_, titles := languageOf("main.py")
	assert.True(t, titles.MatchString("Python Cookbook.pdf"))
	assert.False(t, titles.MatchString("why-rust.pdf"))
	_, titles = languageOf("server.go")
	assert.True(t, titles.MatchString("learning-go-programming.pdf"))
	assert.True(t, titles.MatchString("Go_Programming_-_Tristan_Hurley.pdf"))
	assert.False(t, titles.MatchString("Google-Flutter.pdf"))
	assert.NotNil(t, titleWordsOf("python"))
	assert.Nil(t, titleWordsOf(""))
}

// seedBook puts a finished book of a category into the library of the test user.
func (e *attachmentEnv) seedBook(t *testing.T, name, category string, texts ...string) {
	t.Helper()
	ctx := context.Background()
	file, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: e.userID, FileName: name, FileHash: "hash-" + name,
		MimeType: "application/pdf", FileSize: 1, Status: "completed", LocalPath: "storage/none",
	})
	require.NoError(t, err)
	require.NoError(t, e.db.Queries.UpdateIngestedFileCategoryAuto(ctx, db.UpdateIngestedFileCategoryAutoParams{ID: file.ID, Category: category}))
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

// topicModel is the chat model of a test that also remembers what it was given to read the topic.
type topicModel struct {
	*llm.MockLLM
	topicRead string
}

func (m *topicModel) ChatWithJSONSchema(ctx context.Context, req openai.ChatCompletionRequest, schema interface{}) (openai.ChatCompletionResponse, error) {
	if len(req.Messages) > 1 {
		m.topicRead = req.Messages[1].Content
	}
	return m.MockLLM.ChatWithJSONSchema(ctx, req, schema)
}

// analysisModel reads the topic as topic and answers the turn with "Анализът е готов.".
func analysisModel(t *testing.T, topic docTopic) *topicModel {
	t.Helper()
	raw, err := json.Marshal(topic)
	require.NoError(t, err)
	return &topicModel{MockLLM: &llm.MockLLM{
		Response:     openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: string(raw)}}}},
		StreamChunks: []openai.ChatCompletionStreamResponse{textChunk("Анализът е готов.")},
	}}
}

// analysisOrchestrator is orchestratorWith for a topicModel.
func analysisOrchestrator(t *testing.T, e *attachmentEnv, model *topicModel) *Orchestrator {
	t.Helper()
	o := orchestratorWith(t, e, model.MockLLM)
	o.llmClient = model
	return o
}

// analyse sends a message with the button "Анализ".
func analyse(t *testing.T, o *Orchestrator, e *attachmentEnv, session, message string) string {
	t.Helper()
	body, _ := json.Marshal(ChatRequest{SessionID: session, Content: message, Mode: ModeAnalyze})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()
	o.HandleStreamChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

// lastUserOf is the person's message of the first request to the chat model, with its notes.
func lastUserOf(t *testing.T, model *topicModel) string {
	t.Helper()
	require.NotEmpty(t, model.Requests, "the chat model was asked")
	msgs := model.Requests[0].Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == openai.ChatMessageRoleUser {
			return msgs[i].Content
		}
	}
	t.Fatal("no message of the person")
	return ""
}

const flutterApp = "import asyncio\nimport flet as ft\n\ndef main(page):\n    page.title = 'Lili'\n\nft.app(main)\n"

// Code is analysed by the books of its language: of the programming books, those with the language
// in the title; never a book of another category.
func TestCodeIsAnalysedByTheBooksOfItsLanguage(t *testing.T) {
	e := newAttachmentEnv(t)
	e.seedBook(t, "Python Cookbook.pdf", "programming", "Use asyncio to run coroutines in an event loop.", "Keep the configuration in a module of its own.")
	e.seedBook(t, "learning-go-programming.pdf", "programming", "A goroutine is a lightweight thread.")
	e.seedBook(t, "Freud.pdf", "psychology", "The ego mediates between the id and reality.")
	e.attach(t, e.userID, "a1", "main.py", flutterApp)
	model := analysisModel(t, docTopic{Category: "programming", Keywords: []string{"asyncio", "GUI"}, Language: "python"})

	out := analyse(t, analysisOrchestrator(t, e, model), e, "a1", "анализирай")

	assert.Contains(t, out, "Анализът е готов.")
	note := lastUserOf(t, model)
	assert.Contains(t, note, `the button "Анализ"`)
	assert.Contains(t, note, `"main.py" (7 lines; subject: programming, Python, asyncio, GUI)`)
	assert.Contains(t, note, "Its text is above, in the attached files.", "a small file is in the prompt already")
	assert.Contains(t, note, "Python Cookbook")
	assert.NotContains(t, note, "goroutine", "a book of another language")
	assert.NotContains(t, note, "ego", "a book of another category")
	assert.Contains(t, note, "4) what the books above say")
	assert.Contains(t, model.topicRead, "import flet as ft", "the topic is read from the file")
	assert.Nil(t, model.Requests[0].ToolChoice, "nothing is forced in an analysis")
}

// An article on psychology is analysed by the books on psychology.
func TestATextIsAnalysedByTheBooksOfItsCategory(t *testing.T) {
	e := newAttachmentEnv(t)
	e.seedBook(t, "Python Cookbook.pdf", "programming", "Use asyncio to run coroutines in an event loop.")
	e.seedBook(t, "Freud.pdf", "psychology", "The ego mediates between the id and reality.", "Dreams are the royal road to the unconscious.")
	e.attach(t, e.userID, "a2", "article.md", "Why we dream: a short article about the unconscious mind and sleep.\n")
	model := analysisModel(t, docTopic{Category: "psychology", Keywords: []string{"dreams", "unconscious"}})

	analyse(t, analysisOrchestrator(t, e, model), e, "a2", "какво мислиш за статията")

	note := lastUserOf(t, model)
	assert.Contains(t, note, "subject: psychology, dreams, unconscious")
	assert.Contains(t, note, "Freud")
	assert.NotContains(t, note, "asyncio")
}

// No book of the subject: the note says so, and no book is named by chance.
func TestAnAnalysisWithoutBooksOfTheSubjectSaysSo(t *testing.T) {
	e := newAttachmentEnv(t)
	e.seedBook(t, "Freud.pdf", "psychology", "The ego mediates between the id and reality.")
	e.attach(t, e.userID, "a3", "main.py", flutterApp)
	model := analysisModel(t, docTopic{Category: "programming", Keywords: []string{"asyncio"}, Language: "python"})

	analyse(t, analysisOrchestrator(t, e, model), e, "a3", "анализирай")

	note := lastUserOf(t, model)
	assert.Contains(t, note, "The person's library has no book on this subject")
	assert.NotContains(t, note, "Freud")
}

// A file too big for the prompt: its first part goes into the note.
func TestABigFileIsAnalysedFromItsFirstPart(t *testing.T) {
	e := newAttachmentEnv(t)
	e.attach(t, e.userID, "a4", "huge.go", bigFile())
	model := analysisModel(t, docTopic{Category: "programming", Keywords: []string{"handlers"}, Language: "go"})

	analyse(t, analysisOrchestrator(t, e, model), e, "a4", "анализирай")

	note := lastUserOf(t, model)
	assert.Contains(t, note, "of 1500 follow; read the rest yourself with read_attachment")
	assert.Contains(t, note, "Never tell the person to call a tool.")
	assert.Contains(t, note, "<<< huge.go >>>")
	assert.Contains(t, note, "   1| ")
	assert.Less(t, estimateTokens(note), analyzeDocTokens+1500, "the part is bounded")
}

// Nothing attached: the code says how, and the model is not asked.
func TestAnAnalysisWithoutAFileSaysHow(t *testing.T) {
	e := newAttachmentEnv(t)
	model := analysisModel(t, docTopic{Category: "general"})

	out := analyse(t, analysisOrchestrator(t, e, model), e, "a5", "анализирай")

	assert.Contains(t, out, "За „Анализ“ ми трябва прикачен файл")
	assert.Empty(t, model.Requests, "the chat model was not asked")
}

// The message names one of two files: that one is analysed.
func TestTheFileTheMessageNamesIsAnalysed(t *testing.T) {
	e := newAttachmentEnv(t)
	e.attach(t, e.userID, "a6", "first.py", "print('one')\n")
	e.attach(t, e.userID, "a6", "second.py", "print('two')\n")
	model := analysisModel(t, docTopic{Category: "programming", Language: "python"})

	analyse(t, analysisOrchestrator(t, e, model), e, "a6", "анализирай first.py")

	assert.Contains(t, lastUserOf(t, model), `"first.py"`)
	assert.True(t, strings.Contains(model.topicRead, "print('one')"))
}

// "Какво мислиш за този код" with a file too big for the prompt would read it first (attachment_force.go);
// in an analysis its part is in the note, and nothing is forced.
func TestAnAnalysisForcesNoTool(t *testing.T) {
	e := newAttachmentEnv(t)
	e.attach(t, e.userID, "a7", "huge.go", bigFile())
	model := analysisModel(t, docTopic{Category: "programming", Language: "go"})

	analyse(t, analysisOrchestrator(t, e, model), e, "a7", "какво мислиш за този код")

	require.NotEmpty(t, model.Requests)
	assert.Nil(t, model.Requests[0].ToolChoice)
	assert.Greater(t, len(model.Requests[0].Tools), 1, "all the tools are on offer")
}
