package agent

import (
	"sync"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

const (
	maxContextChars    = 12000
	maxHistoryMessages = 10
)

// ChatRequest defines the expected payload for the stream chat endpoint.
type ChatRequest struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}

type sessionSummary struct {
	mu            sync.Mutex
	text          string
	lastDropCount int
	isGenerating  bool
}

type chatContext struct {
	Memories    []string
	Chunks      []string
	Sources     []api.SourceInfo
	Rules       []string
	TopMemories []string
	History     []openai.ChatCompletionMessage
	RollingSum  string
}
