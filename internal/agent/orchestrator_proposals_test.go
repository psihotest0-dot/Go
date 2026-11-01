package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/proposals"
	"go-monolith/internal/retrieval"
)

// proposalOrchestrator is an orchestrator on the real registry whose model first
// asks for the given tool call and then answers in text.
func proposalOrchestrator(t *testing.T, e *toolEnv, toolName, toolArgs string) *Orchestrator {
	t.Helper()

	previous := prompts.GlobalManager
	prompts.Init("")
	t.Cleanup(func() { prompts.GlobalManager = previous })

	model := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call-1", toolName, toolArgs)},
		{textChunk("Предлагам да го запомня — потвърди ми.")},
	}}
	return NewOrchestrator(e.db, retrieval.NewRetrievalService(e.db, &llm.MockEmbedder{}), model, e.reg)
}

func TestStreamChatReportsProposalsToTheClient(t *testing.T) {
	e := newToolEnv(t)
	o := proposalOrchestrator(t, e, "save_global_rule", `{"key":"language","value":"Говори на български"}`)

	body, _ := json.Marshal(ChatRequest{SessionID: "sess-prop", Content: "Винаги ми говори на български"})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()

	o.HandleStreamChat(rec, req)

	out := rec.Body.String()
	require.Contains(t, out, "event: proposals\ndata: ", "the client is told a decision is waiting")
	assert.Contains(t, out, `"kind":"save_global_rule"`)
	assert.Contains(t, out, "Говори на български")
	assert.Less(t, strings.Index(out, "event: proposals"), strings.Index(out, "data: [DONE]"), "before the stream ends")

	assert.Empty(t, e.rules(t), "the rule is not in the prompt until the user confirms")
	pending := e.pending(t)
	require.Len(t, pending, 1)
	assert.Equal(t, "sess-prop", pending[0].SessionID)
}

func TestStreamChatWithoutProposalsSendsNoProposalEvent(t *testing.T) {
	e := newToolEnv(t)
	o := proposalOrchestrator(t, e, "create_memory", `{"content":"Казва се Иван"}`)

	body, _ := json.Marshal(ChatRequest{SessionID: "sess-clean", Content: "Казвам се Иван"})
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, e.userID))
	rec := httptest.NewRecorder()

	o.HandleStreamChat(rec, req)

	out := rec.Body.String()
	assert.NotContains(t, out, "event: proposals")
	assert.Contains(t, out, "data: [DONE]")
	assert.Len(t, e.memories(t), 1, "a plain remark of the user is remembered at once")
	assert.Empty(t, e.pending(t))
}

func TestProcessChatTurnReturnsTheProposals(t *testing.T) {
	e := newToolEnv(t)
	o := proposalOrchestrator(t, e, "save_global_rule", `{"key":"tone","value":"Бъди кратка"}`)

	turn, err := o.ProcessChatTurn(context.Background(), e.userID, ChatRequest{SessionID: "tg-1", Content: "Говори по-кратко"})

	require.NoError(t, err)
	assert.Equal(t, "Предлагам да го запомня — потвърди ми.", turn.Answer)
	require.Len(t, turn.Proposals, 1)
	assert.Equal(t, proposals.KindSaveRule, turn.Proposals[0].Kind)
	assert.Equal(t, "tg-1", turn.Proposals[0].SessionID)
	assert.Empty(t, e.rules(t))

	// ProcessChat is the same turn without the proposals.
	answer, err := o.ProcessChat(context.Background(), e.userID, ChatRequest{SessionID: "tg-1", Content: "Още веднъж"})
	require.NoError(t, err)
	assert.NotEmpty(t, answer)
}

func TestRegeneratedTurnDoesNotStackTheSameProposal(t *testing.T) {
	e := newToolEnv(t)
	o := proposalOrchestrator(t, e, "save_global_rule", `{"key":"tone","value":"Бъди кратка"}`)

	for i := 0; i < 3; i++ {
		_, err := o.ProcessChatTurn(context.Background(), e.userID, ChatRequest{SessionID: "tg-2", Content: "Говори по-кратко"})
		require.NoError(t, err)
	}

	assert.Len(t, e.pending(t), 1, "asking again is the same proposal")
}
