package agent

import (
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

const (
	// maxContextTokens bounds the conversation history replayed on every turn,
	// measured in estimated tokens (see estimateTokens). The earlier character
	// cap of 12000 was the same budget for Latin text but let Cyrillic history
	// grow to roughly twice the intended context.
	maxContextTokens   = 3000
	maxHistoryMessages = 10

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

// sessionSummary holds the cached rolling summary of a single chat session.
// Entries are keyed per owner (see summaryKey) so identical session IDs from
// different users never share text. text and lastDropCount are guarded by mu.
// isGenerating and lastUsed are atomic because the background summariser and
// the eviction sweep access them without holding mu.
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
