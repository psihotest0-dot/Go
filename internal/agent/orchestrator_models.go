package agent

import (
	"sync"
	"sync/atomic"
	"time"

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

// sessionSummary holds the cached rolling summary of a single chat session.
// text and lastDropCount are guarded by mu. isGenerating and lastUsed are
// atomic because the background summariser and the eviction sweep access them
// without holding mu.
type sessionSummary struct {
	mu            sync.Mutex
	text          string
	lastDropCount int

	isGenerating atomic.Bool
	lastUsed     atomic.Int64 // unix nanoseconds of the last cache access
}

// touch records that the summary was just used so the eviction sweep keeps it.
func (s *sessionSummary) touch(now time.Time) {
	s.lastUsed.Store(now.UnixNano())
}

// lastAccessed reports when the summary was last used.
func (s *sessionSummary) lastAccessed() time.Time {
	return time.Unix(0, s.lastUsed.Load())
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
