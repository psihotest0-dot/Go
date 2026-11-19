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
// code: an answer that claims a write no file tool made is asked again once, with the file
// tools open (retryUnbackedWrite), and an answer that still claims a write or a removal
// that did not happen gets a correction under it.

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

	// retryNote is added to the message of the user, for the model only, when its answer
	// claimed a write that no tool made and the turn is asked again (retryUnbackedWrite).
	retryNote = "[Note for the assistant, not written by the user: your first answer to this message said that a file was written, " +
		"but no file tool was called and nothing was written. Do what the user asked now with the file tools " +
		"(append_file adds to a file that exists, write_file makes a new one; list_files shows what is there), " +
		"then say in one or two sentences what the tools did.]"
	// retryNotice is streamed under the first answer before the second one.
	retryNotice = "\n\n⚠️ Това не беше записано — правя го сега.\n\n"

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
	// appendVerb: the message asks to add to something ("добави", "допиши", "append", "add");
	// createVerb: it says it is a new file. See asksToAppend.
	appendVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:добав|допиш|дописв|допълн|прибав|append|add(?:[^\p{L}]|$))`)
	createVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:създа|нов\p{L}*\s+файл|new\s+file|create|make\s+a\s+file)`)
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

// asksToAppend tells whether a message asks to add to a file and not to make one. A plain
// filter: when the message also speaks of creating or of a new file it is not one.
func asksToAppend(message string) bool {
	return appendVerb.MatchString(message) && !createVerb.MatchString(message)
}

// unbackedFileClaim returns the correction to show under an answer that says a file was
// written, added to or removed when it was not, or "" when there is nothing to correct.
// wrote tells that a file tool wrote or added something in this turn. A removal is never
// backed: no tool does it.
//
// The answer is about a file when it names one, or when askedAboutFile: the message named
// a file and the turn called no tool at all. "Записах го веднага." to "добави във същия
// файл …" names nothing (1 of 4 in the acceptance run of P52, 2026-11-16), and without a
// tool call it cannot be a memory the model saved either.
func unbackedFileClaim(answer string, wrote, askedAboutFile bool) string {
	if !askedAboutFile && !fileMention.MatchString(answer) {
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

// generateGuarded runs the rounds (generateRounds) and checks what the answer says about
// the files. See the comment at the top of this file. Which model runs them is decided
// by generateResponse (model_routing.go), which calls this.
func (o *Orchestrator) generateGuarded(
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
	turn := turnFrom(ctx)
	userNamedFile := fileMention.MatchString(lastUserContent(finalMessages))
	claim := func() string {
		return unbackedFileClaim(content, turn.filesWritten() > 0, userNamedFile && len(executed) == 0)
	}
	if claim() == writeCorrection && !usedFileTools(executed) {
		if open, required := o.retryFileTools(tools, turn); len(required) > 0 {
			// "Add to the file" after a history in which the model said it had deleted it:
			// left a choice the model took write_file (a replacement, which waits for the
			// user's confirmation) instead of append_file. The first round of the retry
			// offers no write_file; the later rounds still do, for a file that is not there.
			if asksToAppend(lastUserContent(finalMessages)) {
				required = withoutTools(required, writeFileTool)
			}
			retried, retriedReasoning, retriedCalls, err := o.retryUnbackedWrite(ctx, messages, open, required, onDelta)
			reasoning += retriedReasoning
			executed = append(executed, retriedCalls...)
			if err != nil {
				return retried, reasoning, executed, err
			}
			content = retried
		}
	}
	if note := claim(); note != "" {
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

// usedFileTools tells whether the turn called any tool of the file group, the stub
// included. Such a turn may have made a proposal or read a file; its claim is only
// corrected, not retried.
func usedFileTools(calls []openai.ToolCall) bool {
	return slices.ContainsFunc(calls, func(c openai.ToolCall) bool {
		switch c.Function.Name {
		case filesStubTool, listFilesTool, readFileTool, writeFileTool, appendFileTool:
			return true
		}
		return false
	})
}

// retryFileTools are the tools of the retry: open is the offer of the turn with the file
// group unlocked, required are the file tools the first round must use. The rule of
// nextTools holds here too: after the web, read_file is not offered. Nothing is returned
// when the stub is not on offer.
func (o *Orchestrator) retryFileTools(tools []openai.Tool, turn *turnState) (open, required []openai.Tool) {
	if !hasTool(tools, filesStubTool) {
		return nil, nil
	}
	open = replaceTool(tools, filesStubTool, fileToolDefinitions())
	if turn.has(sourceWebSearch) || turn.has(sourceWebFetch) {
		open = withoutTools(open, readFileTool)
	}
	return open, addedTools(tools, open)
}

// retryUnbackedWrite asks the model again, when its answer claimed a file was written and
// no file tool was called: the file group is open and the first round must call one of
// its tools (plan item P52).
//
// Why: in a conversation where a file was made, the history keeps only the answer's
// words ("Създадох файла test_file.txt…"), not the tool call, and the group is locked
// again in the next turn. The model copied its own words: "допиши във същия файл" got
// "Добавих…" with no call in 12 of 12 replays of the real Telegram context (2026-11-16);
// a correction under the answer told the user, but every false "Добавих" made the next
// turn worse.
//
// The client has already seen the first answer as it streamed: a line under it says it
// was not written and the second answer follows. What is returned, and stored, is the
// second answer alone, so that the history does not keep the false claim.
func (o *Orchestrator) retryUnbackedWrite(
	ctx context.Context,
	messages []openai.ChatCompletionMessage,
	open, required []openai.Tool,
	onDelta deltaFunc,
) (string, string, []openai.ToolCall, error) {
	slog.WarnContext(ctx, "the answer claims a file write that no tool made; the turn is asked again with the file tools")
	metrics.UserFileOps.WithLabelValues("claim", "retried").Inc()
	if onDelta != nil {
		if err := onDelta(retryNotice, ""); err != nil {
			return "", "", nil, err
		}
	}
	return o.runRounds(ctx, withNoteOnLastUser(messages, retryNote), open, onDelta, required)
}
