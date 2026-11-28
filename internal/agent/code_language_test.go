package agent

import (
	"testing"

	"github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
)

func TestAttachedCodeLanguageIsTheOneLanguageOfTheCodeFiles(t *testing.T) {
	assert.Equal(t, "Python", attachedCodeLanguage([]string{"main.py"}))
	assert.Equal(t, "Python", attachedCodeLanguage([]string{"notes.txt", "views/chat.py", "MAIN.PY"}), "other files and the case do not matter")
	assert.Equal(t, "Go", attachedCodeLanguage([]string{"orchestrator.go", "app.log"}))
	assert.Empty(t, attachedCodeLanguage([]string{"main.py", "server.go"}), "two languages: none is chosen")
	assert.Empty(t, attachedCodeLanguage([]string{"notes.txt", "data.csv"}), "no code")
	assert.Empty(t, attachedCodeLanguage(nil))
}

func TestWithCodeLanguageAddsTheLanguageOnlyToAQueryAboutProgrammingThatNamesNone(t *testing.T) {
	cases := []struct{ query, want, why string }{
		// The three queries of the Flet turn of 2026-11-26.
		{"software engineering principles clean code architecture", "software engineering principles clean code architecture Python", "the query of the live turn"},
		{"software architecture principles clean code design patterns", "software architecture principles clean code design patterns Python", ""},
		{"как да преструктурирам кода", "как да преструктурирам кода Python", "a Bulgarian query"},
		{"рефакториране на приложение", "рефакториране на приложение Python", ""},
		// The advice of the same conversation is no question about code.
		{"success personal development advice", "success personal development advice", "a query not about programming"},
		{"принципи за успех в живота", "принципи за успех в живота", ""},
		// A language already named stays the only one.
		{"clean code in Python", "clean code in Python", "the language is there"},
		{"Rust ownership and modules", "Rust ownership and modules", "another language is asked for"},
		{"async code with node.js", "async code with node.js", "a runtime names its language"},
		{"C++ code structure", "C++ code structure", ""},
		{"", "", "an empty query"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, withCodeLanguage(c.query, "Python"), "%q: %s", c.query, c.why)
	}
	assert.Equal(t, "clean code architecture", withCodeLanguage("clean code architecture", ""), "no attached code")
}

// The search itself: what the model wrote, with the language of the attached code, both queries.
func TestSearchDocumentsAddsTheLanguageOfTheAttachedCode(t *testing.T) {
	e := newToolEnv(t)
	log := &embedderLog{}
	reg := NewDefaultToolRegistry(retrieval.NewRetrievalService(e.db, log), e.db, e.svc)

	run := func(language string, args map[string]interface{}) []string {
		ctx, turn := e.turn("s", false)
		turn.setCodeLanguage(language)
		before := len(log.seen())
		res, err := reg.ExecuteTool(ctx, "search_documents", args)
		require.NoError(t, err)
		assert.Equal(t, "success", resultMap(t, res)["status"])
		return log.seen()[before:]
	}

	assert.Equal(t,
		[]string{"как да преструктурирам кода Python", "software engineering principles clean code architecture Python"},
		run("Python", map[string]interface{}{"query": "как да преструктурирам кода", "query_en": "software engineering principles clean code architecture"}),
		"both queries get the language")
	assert.Equal(t,
		[]string{"software engineering principles clean code architecture"},
		run("", map[string]interface{}{"query": "software engineering principles clean code architecture"}),
		"no attached code: the query as written")
	assert.Equal(t,
		[]string{"success personal development advice"},
		run("Python", map[string]interface{}{"query": "success personal development advice"}),
		"a query not about programming: as written")
}

// The whole turn: the language comes from the files attached to the conversation.
func TestASearchInAConversationWithAttachedPythonGetsPython(t *testing.T) {
	e := newAttachmentEnv(t)
	log := &embedderLog{}
	reg := NewDefaultToolRegistry(retrieval.NewRetrievalService(e.db, log), e.db, e.toolEnv.svc)
	reg.WithAttachments(e.svc)
	// Two turns: each searches, then answers.
	search := []openai.ChatCompletionStreamResponse{toolCallChunk(0, "call-1", searchDocumentsTool, `{"query": "software architecture clean code"}`)}
	answer := []openai.ChatCompletionStreamResponse{textChunk("Ето какво казват книгите.")}
	model := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{search, answer, search, answer}}
	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })
	o := NewOrchestrator(e.db, retrieval.NewRetrievalService(e.db, &llm.MockEmbedder{}), model, reg).WithAttachments(e.svc)

	e.attach(t, e.userID, "chat-py", "main.py", "def main():\n    pass\n")
	askAboutFiles(t, o, e.userID, "chat-py")
	assert.Equal(t, []string{"software architecture clean code Python"}, log.seen())

	// Another conversation, with no code attached: the query as the model wrote it.
	askAboutFiles(t, o, e.userID, "chat-none")
	assert.Equal(t, []string{"software architecture clean code Python", "software architecture clean code"}, log.seen())
}
