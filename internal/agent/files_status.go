package agent

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

// "Успя ли?" - a question about how far some work is, asked after a turn in which the model
// said it would do it - gets no help from the history: it keeps the words of the turns, not
// their tool calls. On 8 replays of the real turn of 2026-11-20, 19:06, the model said "Да,
// успях да извлека… Файлът вече съдържа по-пълно описание" in 4 and said "Не успях" in 3,
// with no tool call in any, and the file had not been touched since 19:00 (plan item P54).
//
// What can be checked is the folder: every file has the time of its last change. A file
// that nothing has changed since the user's earlier messages cannot have been written by
// the work the answer says is done.

const (
	// statusCorrection goes under an answer that says the work is done when no file of the
	// user was changed since their earlier messages.
	statusCorrection = "⚠️ Поправка: във файловете няма никаква промяна от последните ти молби — нищо не е записано. " +
		"Кажи ми какво да направя и го правя в самия отговор."
	// minRequestRunes: a message of the user shorter than this ("ok", "добре", "да оставим
	// това") is not a request for work; the check goes back past it.
	minRequestRunes = 20
)

// notDone: the assistant says the work was not done ("Не успях", "още не е готово").
var notDone = regexp.MustCompile(`(?i)(?:не\s+(?:успях|успех|успя|съм\s+успял\p{L}*|е\s+готов\p{L}*|са\s+готов\p{L}*)|все\s+още\s+не|още\s+не)`)

// filesChangeChecker is a tool registry that can tell whether the user's files changed.
type filesChangeChecker interface {
	FilesChangedSince(userID uuid.UUID, since time.Time) (bool, error)
}

// FilesChangedSince tells whether any file of the user was changed at or after since.
func (r *DefaultToolRegistry) FilesChangedSince(userID uuid.UUID, since time.Time) (bool, error) {
	if r.files == nil {
		return false, nil
	}
	list, err := r.files.List(userID)
	if err != nil {
		return false, err
	}
	for _, f := range list {
		if !f.Modified.Before(since) {
			return true, nil
		}
	}
	return false, nil
}

// requestSince is the time from which a change of a file counts for the work a status
// question asks about: the time of the last message of the user before the question that is
// long enough to be a request (minRequestRunes), or the time of the earliest message before it
// when none is. The zero time when there is no earlier message, or the times do not go with
// the messages. (First tried: two messages back; on the real chat of 2026-11-20 that went back
// to the request that had made the file, 18:00, and the claim of 19:06 passed.)
func requestSince(history []openai.ChatCompletionMessage, at []time.Time) time.Time {
	if len(at) != len(history) {
		return time.Time{}
	}
	var users []int
	for i, m := range history {
		if m.Role == openai.ChatMessageRoleUser {
			users = append(users, i)
		}
	}
	if len(users) < 2 {
		return time.Time{}
	}
	earlier := users[:len(users)-1] // without the question itself
	for i := len(earlier) - 1; i >= 0; i-- {
		if utf8.RuneCountInString(strings.TrimSpace(history[earlier[i]].Content)) >= minRequestRunes {
			return at[earlier[i]]
		}
	}
	return at[earlier[0]]
}

// statusClaimUnbacked tells whether the answer to a question about how far the work is
// says that it is done, while nothing was called in the turn and no file of the user was
// changed since their earlier messages. The answer must be about a file, and must neither
// ask, refuse, say "не успях" nor promise (those are not claims of success).
func (o *Orchestrator) statusClaimUnbacked(ctx context.Context, turn *turnState, userMessage, answer string, called bool) bool {
	if called || !asksForStatus(userMessage) || !leavesTheWork(answer) || notDone.MatchString(answer) {
		return false
	}
	if !fileMention.MatchString(answer) && !fileMention.MatchString(userMessage) && !asksToSave(userMessage) {
		return false
	}
	since := turn.requestSinceTime()
	checker, ok := o.tools.(filesChangeChecker)
	userID, hasUser := ctx.Value(api.ParsedUserIDKey).(uuid.UUID)
	if since.IsZero() || !ok || !hasUser {
		return false
	}
	changed, err := checker.FilesChangedSince(userID, since)
	return err == nil && !changed
}
