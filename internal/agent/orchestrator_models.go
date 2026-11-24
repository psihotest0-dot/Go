package agent

import (
	"time"

	"github.com/google/uuid"
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
	// UsePaid is the owner's choice of the model for this turn: true - the paid one (the
	// button "cloud" of the client), false - the local one even for a library question, left
	// out - the default (the local model; library questions follow LIBRARY_LLM). It does
	// nothing while the paid provider is off.
	UsePaid *bool `json:"use_paid,omitempty"`
	// QueueEvents asks for the frame "event: queued" when the conversation is taken by another
	// reply and this turn has to wait. A client that does not know the frame must not ask for
	// it: it would take the frame for text.
	QueueEvents bool `json:"queue_events,omitempty"`
	// PlanEvents asks for the frame "event: plan" (data: {"remembered": [...]}) after the text of
	// an answer that used the person's memories (П55, step 2.5), for the line "Лили си спомни".
	// A client that does not know the frame must not ask for it.
	PlanEvents bool `json:"plan_events,omitempty"`
	// Mode is the button the person chose for this message (turn_mode.go): "books", "web",
	// "library", "files", "memory" or "style" - the turn must call a tool of it. Left out: none.
	Mode string `json:"mode,omitempty"`
}

type chatContext struct {
	Memories []string
	// MemoryHits are the memories the search found for the message, the ones of the profile
	// (TopMemories) too: the plan of an answer (answer_plan.go) chooses among them, and the
	// memory that fits "уморен съм" best may well be one the prompt already holds as a profile.
	MemoryHits []uuid.UUID
	Chunks     []string
	Sources    []api.SourceInfo
	Rules      []string
	// Lean leaves out of the system prompt the parts Orchestrator.WithLeanPrompt names.
	Lean bool
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
	// EarlierAnswer is the last answer of the conversation that rested on passages of the
	// library (earlier_answer.go).
	EarlierAnswer earlierAnswer
}
