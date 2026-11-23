package agent

import (
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/prompts"
)

// The prompt cache of the model servers (plan step 2.11). A server reuses the start of a
// request that is the same as the start of the one before; the local Gemma (sliding-window
// attention, two context checkpoints in Lemonade) only when the new request is the old one with
// more at its end. The memories and the book summaries found for a turn stood in the system
// prompt, before the history, so every turn with other ones made the whole history be read again.
//
// contextAfterHistory (CONTEXT_AFTER_HISTORY_ENABLED) puts them in a system message of their own
// right before the person's last message; the system prompt at the start is then the same in
// every turn. The message is in the copy the model reads only: the stored history keeps the
// person's words, so the next turn has the same start up to there. A system message, not words
// added to the person's message: the checks of the turn read that message as he wrote it
// (forcedFirstTool, the explanation, "успя ли?", the limits of length).
//
// Measured on 2026-11-23 (go-monolith-backups\measurements\cache_2026-11-23):
//   - the cache: the owner's Flet conversation, 52 turns, each with what it really had - the local
//     Gemma 26 % of the prompt from the cache -> 56 % (the prompt time 115 s -> 74 s); an invented
//     conversation of 30 turns on DeepSeek 62 % -> 88 % (the input 65 % cheaper);
//   - the answers of the real Gemma: 25 personal messages with 3 memories each - no answer took the
//     context for the person's words, a memory woven into the answer 13 of 50 instead of 7 of 50
//     (read: they fit); 20 questions to the books - searched 40 of 40 as before; a question in
//     English is now answered in Bulgarian, as the prompt says ("Respond in Bulgarian, unless the
//     user requests another language"), where before it was answered in English; one question to
//     the books that is not forced ("…? Запомни, че…") was answered from the summaries without a
//     search, 2 of 2 (searched 2 of 2 before).
//
// Not done, measured the same day: the rounds after a forced search offering that tool alone (the
// tools are part of the start of the request, after the system prompt, and the round after the
// forced one offers all of them again). It took 2 s off a question to the books, but a question
// that also asks for a file or the web lost it: a file was written 3 of 8 instead of 8 of 8, the
// web 0 of 2 instead of 2 of 2. Nor all the tools in the forced round: with tool_choice "required"
// Gemma searched the books for 32 of 43 questions to the books and went to the paid web search
// for most of the others.

// WithContextAfterHistory puts the memories and the summaries of a turn after the history.
func (o *Orchestrator) WithContextAfterHistory() *Orchestrator {
	o.contextAfterHistory = true
	return o
}

// takeTurnContext renders the memories and the summaries of the turn as the message that goes
// before the person's last one, and takes them out of the context of the system prompt. It
// returns nil (and leaves the context as it is) when there is nothing to move or the last
// message of the history the model reads is not the person's.
func takeTurnContext(c *chatContext, history []openai.ChatCompletionMessage) (*openai.ChatCompletionMessage, error) {
	if len(c.Memories) == 0 && len(c.Chunks) == 0 {
		return nil, nil
	}
	if n := len(history); n == 0 || history[n-1].Role != openai.ChatMessageRoleUser {
		return nil, nil
	}
	text, err := prompts.GlobalManager.Render("chat_turn_context.tmpl", map[string]interface{}{
		"RelevantMemories": c.Memories,
		"Chunks":           c.Chunks,
	})
	if err != nil {
		return nil, err
	}
	c.Memories, c.Chunks = nil, nil
	return &openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: strings.TrimSpace(text)}, nil
}

// withTurnContext is the messages of the turn with the context message right before the last
// one (the person's message: takeTurnContext made it only then).
func withTurnContext(messages []openai.ChatCompletionMessage, turnContext *openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	if turnContext == nil || len(messages) == 0 {
		return messages
	}
	last := len(messages) - 1
	out := make([]openai.ChatCompletionMessage, 0, len(messages)+1)
	out = append(out, messages[:last]...)
	out = append(out, *turnContext, messages[last])
	return out
}
