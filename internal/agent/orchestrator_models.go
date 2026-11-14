package agent

import (
	"time"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

const (
	// maxChatBodyBytes bounds the chat and regenerate request bodies before they
	// are decoded, so a malformed client cannot make the process allocate an
	// arbitrarily large buffer.
	maxChatBodyBytes = 1 << 20

	// maxUserMessageTokens bounds a single user turn, measured with the same
	// estimateTokens used by the history budget. Anything larger than the whole
	// context budget is pruned before it can be answered, so rejecting it up
	// front is clearer than silently dropping the question.
	maxUserMessageTokens = 4000
)

// ChatRequest defines the expected payload for the stream chat endpoint.
type ChatRequest struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}

type chatContext struct {
	Memories []string
	Chunks   []string
	Sources  []api.SourceInfo
	Rules    []string
	// StyleNotes are the confirmed notes on how the user likes to be talked to.
	StyleNotes  []string
	TopMemories []string
	History     []openai.ChatCompletionMessage
	// HistoryAt holds the creation time of each History message, index for
	// index. It tells the rolling summary where the prompt begins.
	HistoryAt  []time.Time
	RollingSum string
	// Attachments are the files the user attached to the conversation, as the
	// system prompt shows them.
	Attachments []promptFile
}
