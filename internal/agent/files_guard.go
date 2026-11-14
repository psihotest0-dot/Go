package agent

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/metrics"
)

// Two things the model gets wrong about the files, found in the first real chat with the
// file tools (2026-11-14, 20:24-20:26):
//
//   - asked to delete the file, it answered "Разбрах, изтрих файла" - there is no tool that
//     deletes, and the file was untouched. The description of the stub did not say so;
//   - the next two requests ("добави във същия файл", "опитай отново") were answered "Записах
//     го" and "Вече е там" with no tool called at all: the model went on from its own
//     false statement, and the file never got the line.
//
// The first is told to the model, when the message asks for it (a note on that one request,
// not in every prompt). The second cannot be prevented by a prompt, so it is checked in
// code and shown: an answer that claims a write or a removal that did not happen gets a
// correction under it.

const (
	// removalNote is added to the message of the user, for the model only, when the message
	// asks to delete, rename or move a file.
	removalNote = "[Note for the assistant, not written by the user: you have no tool that deletes, renames or moves a file, and nothing was changed. " +
		"Say so plainly, in the user's language, and do not claim you did it. The user can delete the notes you saved in the tab «Файлове на Лили» (the bin icon) and the books in the tab «Файлове».]"

	// removalCorrection and writeCorrection go under an answer that claims what did not happen.
	removalCorrection = "⚠️ Поправка: нямам инструмент за изтриване или преименуване на файлове и нищо не е изтрито. " +
		"Можеш да го направиш сам от таба „Файлове на Лили“ (кошчето)."
	writeCorrection = "⚠️ Поправка: в това съобщение нищо не е записано и не е добавено във файл — не е извикан инструмент за файлове. " +
		"Кажи ми пак какво да направя."

	maxRemovalQuestionRunes = 400
)

var (
	// removalVerb: delete, remove, rename, move, in Bulgarian and English. A whole word or a
	// stem ("изтри", "изтрий", "изтриеш", "премахн").
	removalVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:изтри|изтрив|премахн|махни|маха|унищож|преименува|премести|delete|remove|erase|rename|move)`)
	// removalObject: a file, a folder, a note, or a name with an extension of the file tools.
	removalObject = regexp.MustCompile(`(?i)(?:файл|папк|бележк|\.(?:txt|md|csv|json|ya?ml|log)(?:[^\p{L}]|$)|(?:^|[^\p{L}])(?:files?|folder|notes?)(?:[^\p{L}]|$))`)

	// removalClaim: the assistant says it deleted, removed or renamed something.
	removalClaim = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:изтрих|премахнах|преименувах|унищожих|deleted|removed)(?:[^\p{L}]|$)`)
	// writeClaim: it says it wrote or added something.
	writeClaim = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:записах|запазих|съхраних|добавих|прехвърлих|saved|wrote)(?:[^\p{L}]|$)`)
	// fileMention: the sentence is about a file.
	fileMention = regexp.MustCompile(`(?i)(?:файл|папк|\.(?:txt|md|csv|json|ya?ml|log)(?:[^\p{L}]|$)|(?:^|[^\p{L}])(?:file|folder)(?:[^\p{L}]|$))`)
)

// asksToRemoveFile tells whether a message asks to delete, rename or move a file. A plain
// filter, not a judgement: a false alarm only adds a note that nothing was changed.
func asksToRemoveFile(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" || utf8.RuneCountInString(message) > maxRemovalQuestionRunes {
		return false
	}
	return removalVerb.MatchString(message) && removalObject.MatchString(message)
}

// unbackedFileClaim returns the correction to show under an answer that says a file was
// written, added to or removed when it was not, or "" when there is nothing to correct.
// wrote tells that a file tool wrote or added something in this turn. A removal is never
// backed: no tool does it.
func unbackedFileClaim(answer string, wrote bool) string {
	if !fileMention.MatchString(answer) {
		return ""
	}
	switch {
	case removalClaim.MatchString(answer):
		return removalCorrection
	case !wrote && writeClaim.MatchString(answer):
		return writeCorrection
	}
	return ""
}

// hasFileGroup tells whether the file tools are on offer in this orchestrator.
func (o *Orchestrator) hasFileGroup() bool {
	return slices.ContainsFunc(o.groups, func(g toolGroup) bool { return g.name == "files" })
}

// withNoteOnLastUser returns the messages with the note under the text of the last user
// message. The slice and the message of the caller are not changed: the note is for this
// request to the model only and is not stored.
func withNoteOnLastUser(messages []openai.ChatCompletionMessage, note string) []openai.ChatCompletionMessage {
	out := slices.Clone(messages)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == openai.ChatMessageRoleUser {
			out[i].Content += "\n\n" + note
			break
		}
	}
	return out
}

// generateResponse runs the rounds (generateRounds) and checks what the answer says about
// the files. See the comment at the top of this file.
func (o *Orchestrator) generateResponse(
	ctx context.Context,
	finalMessages []openai.ChatCompletionMessage,
	tools []openai.Tool,
	onDelta deltaFunc,
) (string, string, []openai.ToolCall, error) {
	if !o.hasFileGroup() {
		return o.generateRounds(ctx, finalMessages, tools, onDelta)
	}

	messages := finalMessages
	if asksToRemoveFile(lastUserContent(finalMessages)) {
		metrics.UserFileOps.WithLabelValues("removal", "note").Inc()
		messages = withNoteOnLastUser(finalMessages, removalNote)
	}

	content, reasoning, executed, err := o.generateRounds(ctx, messages, tools, onDelta)
	if err != nil {
		return content, reasoning, executed, err
	}
	if note := unbackedFileClaim(content, turnFrom(ctx).filesWritten() > 0); note != "" {
		slog.WarnContext(ctx, "the answer claims a change of the files that did not happen; a correction is added",
			"answer_runes", utf8.RuneCountInString(content))
		metrics.UserFileOps.WithLabelValues("claim", "corrected").Inc()
		content += "\n\n" + note
		if onDelta != nil {
			if err := onDelta("\n\n"+note, ""); err != nil {
				return content, reasoning, executed, err
			}
		}
	}
	return content, reasoning, executed, nil
}
