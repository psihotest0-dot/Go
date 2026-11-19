package agent

import (
	"sort"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// dropTrailingAssistantMessages removes assistant replies from the end of the
// history. Regenerating keeps the previous answer in the database until a new
// one is ready, so it has to be dropped from the prompt: otherwise the model is
// asked to answer a question it has already answered, with that answer in view.
func dropTrailingAssistantMessages(historyMessages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	end := len(historyMessages)
	for end > 0 && historyMessages[end-1].Role == openai.ChatMessageRoleAssistant {
		end--
	}
	if end == len(historyMessages) {
		return historyMessages
	}
	return historyMessages[:end]
}

// pruneHistory keeps the newest messages that fit maxTokens (estimated) and
// maxMessages. The newest message is always kept, however large. What is left
// out is not lost: the rolling summary picks it up (see rollingSummary).
func pruneHistory(historyMessages []openai.ChatCompletionMessage, maxTokens, maxMessages int) []openai.ChatCompletionMessage {
	var reversedKeep []openai.ChatCompletionMessage
	currentTokens := 0

	for i := len(historyMessages) - 1; i >= 0; i-- {
		msgTokens := estimateTokens(historyMessages[i].Content)

		if i != len(historyMessages)-1 {
			if (currentTokens+msgTokens > maxTokens && len(reversedKeep) > 0) || len(reversedKeep) >= maxMessages {
				break
			}
		}
		reversedKeep = append(reversedKeep, historyMessages[i])
		currentTokens += msgTokens
	}

	finalHistory := make([]openai.ChatCompletionMessage, 0, len(reversedKeep))
	for i := len(reversedKeep) - 1; i >= 0; i-- {
		finalHistory = append(finalHistory, reversedKeep[i])
	}
	return finalHistory
}

// historyCutPercent is how much of its room the history window keeps when it has
// to be cut: the window then has room to grow for several turns before the next cut.
const historyCutPercent = 60

// maxTrackedWindows bounds the memory of historyWindows.
const maxTrackedWindows = 512

// historyWindows remembers, per session, the time of the oldest history message that
// the last prompt began with. The zero value is ready to use.
//
// Why: pruneHistory alone keeps "the newest messages that fit", so once a chat is
// long enough the oldest message falls out on every turn and the history starts
// somewhere else each time. A model server reuses the cached start of the previous
// prompt only up to the first difference; a history that starts elsewhere every turn
// means the whole history is read again every turn (about 3 s on the local model,
// and no cached input at the paid one). Keeping the start until the window is full
// and then cutting it back in one step makes the prompt only grow in between.
type historyWindows struct {
	mu    sync.Mutex
	start map[string]time.Time
	order []string // keys in the order they were first stored, to evict the oldest
}

func (w *historyWindows) get(key string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.start[key]
	return t, ok
}

func (w *historyWindows) put(key string, t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.start == nil {
		w.start = make(map[string]time.Time)
	}
	if _, known := w.start[key]; !known {
		w.order = append(w.order, key)
		if len(w.order) > maxTrackedWindows {
			delete(w.start, w.order[0])
			w.order = w.order[1:]
		}
	}
	w.start[key] = t
}

// fitsHistory reports whether the messages are within both limits.
func fitsHistory(messages []openai.ChatCompletionMessage, maxTokens, maxMessages int) bool {
	if len(messages) > maxMessages {
		return false
	}
	tokens := 0
	for _, m := range messages {
		tokens += estimateTokens(m.Content)
	}
	return tokens <= maxTokens
}

// pruneHistorySteady is pruneHistory that keeps the window where it was. prev is the
// start of the window of the last turn of the session (hasPrev false: none known);
// at holds the time of each message. It returns the messages for the prompt and the
// start to remember (the zero time when the messages carry no times).
//
//   - Without a previous start the window is the newest messages that fit, as before.
//   - With one, the window stays as it was (it only grows by the new messages) as long
//     as it fits. The previous start must still be among the messages that were loaded:
//     the loader reads only the newest ones, so a start older than all of them means the
//     window has outgrown what is read and has to be cut.
//   - When it no longer fits, it is cut to historyCutPercent of the limits.
func pruneHistorySteady(history []openai.ChatCompletionMessage, at []time.Time, prev time.Time, hasPrev bool, maxTokens, maxMessages int) ([]openai.ChatCompletionMessage, time.Time) {
	timed := len(history) > 0 && len(at) == len(history)

	if timed && hasPrev && !prev.Before(at[0]) {
		if i := sort.Search(len(at), func(i int) bool { return !at[i].Before(prev) }); i < len(history) {
			kept := history[i:]
			if len(kept) == 1 || fitsHistory(kept, maxTokens, maxMessages) {
				return kept, at[i]
			}
		}
	}

	limitTokens, limitMessages := maxTokens, maxMessages
	if timed && hasPrev {
		limitTokens = maxTokens * historyCutPercent / 100
		limitMessages = max(1, maxMessages*historyCutPercent/100)
	}
	kept := pruneHistory(history, limitTokens, limitMessages)
	if !timed {
		return kept, time.Time{}
	}
	return kept, at[len(at)-len(kept)]
}

// windowStart is the time of the oldest message of the prompt after pruning:
// the history is cut to its newest keptCount messages, so that is the keptCount-th
// from the end. It is the zero time when the history carries no timestamps.
func (c chatContext) windowStart(keptCount int) time.Time {
	if keptCount <= 0 || len(c.HistoryAt) != len(c.History) || keptCount > len(c.HistoryAt) {
		return time.Time{}
	}
	return c.HistoryAt[len(c.HistoryAt)-keptCount]
}
