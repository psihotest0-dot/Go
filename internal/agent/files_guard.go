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
// tools open (retryUnbacked), and an answer that still claims a write or a removal
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
	// claimed a write that no tool made and the turn is asked again (retryUnbacked).
	retryNote = "[Note for the assistant, not written by the user: your first answer to this message said that a file was written, " +
		"but no file tool was called and nothing was written. Do what the user asked now with the file tools " +
		"(append_file adds to a file that exists, write_file makes a new one; list_files shows what is there), " +
		"then say in one or two sentences what the tools did.]"
	// retryNotice is streamed under the first answer before the second one.
	retryNotice = "\n\n⚠️ Това не беше записано — правя го сега.\n\n"

	// stallNote and stallNotice are the same for an answer that did not claim a write but
	// promised the work for later or said it was going on (plan item P53): "Ще започна
	// веднага да пренаписвам файла… Когато завърша, ще ти кажа." with no tool called.
	stallNote = "[Note for the assistant, not written by the user: your first answer to this message promised to do the work later " +
		"or said that it was going on, but no tool was called and nothing was done. Nothing runs between two messages. " +
		"Do what the user asked now with the file tools (append_file adds to a file that exists, write_file makes a new one; " +
		"list_files shows what is there), then say in one or two sentences what the tools did.]"
	stallNotice = "\n\n⚠️ Обещах го, но нищо не беше направено — правя го сега.\n\n"
	// stallCorrection goes under an answer that still promises work for later or says it
	// is going on, after no tool was called. Nothing happens between two messages.
	stallCorrection = "⚠️ Поправка: не работя на заден план — нищо не се прави между съобщенията ми и в този отговор не е направено нищо. " +
		"Кажи ми какво да направя и го правя в самия отговор."
	// maxStallAnswerRunes: a long answer has done its work in words; a stray phrase in it is
	// not a promise.
	maxStallAnswerRunes = 1500

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
	// "създадох" and the rest were added on 2026-11-20: asked to "create the file first",
	// the model answered "Създадох файла …" with no call in 8 of 8 replays of the real chat.
	// Such a word counts only in an answer about a file (unbackedFileClaim).
	writeClaim = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:записах|запазих|съхраних|добавих|прехвърлих|създадох|направих|подготвих|изготвих|пренаписах|презаписах|обнових|замених|замянах|saved|wrote|created)(?:[^\p{L}]|$)`)
	// appendVerb: the message asks to add to something ("добави", "допиши", "append", "add");
	// createVerb: it says it is a new file. See asksToAppend.
	appendVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:добав|допиш|дописв|допълн|прибав|append|add(?:[^\p{L}]|$))`)
	createVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:създа|нов\p{L}*\s+файл|new\s+file|create|make\s+a\s+file)`)
	// fileMention: the sentence is about a file.
	fileMention = regexp.MustCompile(`(?i)(?:файл|папк|\.(?:txt|md|csv|json|ya?ml|log)(?:[^\p{L}]|$)|(?:^|[^\p{L}])(?:file|folder)(?:[^\p{L}]|$))`)

	// workInProgress: the assistant says that it is working on something now, or that it
	// will tell when it is done. No tool was called, so nothing is going on: nothing runs
	// between two messages. First person only - "в процес на" alone is also how a text says
	// that a city is being rebuilt.
	workInProgress = regexp.MustCompile(`(?i)(?:` +
		`(?:съм|сме)\s+в\s+процес\s+на|` +
		`в\s+момента\s+(?:работя|обработвам|пиша|пренаписвам|извличам|преобразувам|записвам|подготвям|изготвям)|` +
		`(?:още|все\s+още)\s+(?:обработвам|пиша|пренаписвам|извличам|преобразувам|записвам|подготвям)|` +
		`(?:щом|когато|след\s+като)\s+(?:завърша|приключа|свърша|довърша)|` +
		`(?:щом|когато|след\s+като)\s+(?:е|са|бъде|бъдат)\s+готов\p{L}*|` +
		`i['’]m\s+(?:currently\s+)?(?:working\s+on|processing|in\s+the\s+process)|` +
		`(?:when|once|as\s+soon\s+as)\s+i(?:['’]m|\s+am|\s+have)?\s+(?:done|finished)|` +
		`i['’]ll\s+let\s+you\s+know\s+(?:when|once|as)` +
		`)`)
	// workStartsNow: "ще започна веднага", "започвам сега" - a promise to start. After the
	// answer is over it is a promise of work no one does, but "ще го запиша веднага щом ми
	// кажеш името" is a condition and not one (promisesWork looks at what follows).
	workStartsNow = regexp.MustCompile(`(?i)(?:` +
		`ще\s+(?:започна|го\s+направя|го\s+запиша|го\s+запазя|се\s+заема|продължа|пренапиша|запиша)\s+(?:веднага|сега|незабавно)|` +
		`започвам\s+(?:веднага|сега|незабавно)|` +
		`i['’]ll\s+(?:start|begin|do\s+it|get\s+started)\s+(?:right\s+away|now|immediately)` +
		`)`)
	// afterCondition: what follows "веднага" when it is the start of a condition.
	afterCondition = regexp.MustCompile(`(?i)^\s+(?:щом|след|като|когато|ако|as\s+soon|once|when|after)`)
	// statusQuestion: the message asks how far the work is ("успя ли?"). Asking the turn
	// again with the file tools would not answer it; the answer must be honest.
	statusQuestion = regexp.MustCompile(`(?i)(?:успя\s+ли|успяла\s+ли|успя\p{L}*\s+ли|готов\p{L}*\s+ли|готово\s+ли|свърши\s+ли|приключи\s+ли|как\s+върви|докъде\s+(?:стигна|си|сме)|` +
		`did\s+you\s+(?:finish|manage|do)|is\s+it\s+(?:done|ready)|any\s+progress)`)
	// saveVerb: the message asks to keep something ("запиши цялата статия", "запази го"):
	// a request for the file tools even where no file is named. "напиши" and "създай" are
	// not here: they ask for a text as often as for a file.
	saveVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:запиш|запаз|съхран|допиш|дописв|save|append)`)
	// refusal: the assistant says it cannot, or has nothing to do it with.
	refusal = regexp.MustCompile(`(?i)(?:не\s+мога|нямам|не\s+разполагам|не\s+е\s+възможно|не\s+съм\s+в\s+състояние|i\s+can['’]?t|cannot|i\s+don['’]?t\s+have|unable\s+to)`)
	// asksTheUser: the answer asks for what it needs ("кажи ми името на файла") without a
	// question mark.
	asksTheUser = regexp.MustCompile(`(?i)(?:кажи\s+ми|кажете\s+ми|дай\s+ми|уточни|посочи|избери|изпрати\s+ми|прати\s+ми|напиши\s+ми|tell\s+me|let\s+me\s+know|please\s+(?:specify|provide|send))`)
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

// promisesWork tells whether an answer promises work for later or says that it is going
// on - "Ще започна веднага да пренаписвам файла… Когато завърша, ще ти кажа.", "В момента
// съм в процес на преобразуване" (the real answers of 2026-11-20, 19:01 and 19:06) - and
// is not an answer that asks the user something it needs. Meant to be called for an answer
// after which no tool was called: then nothing is going on, and nothing will. A plain
// filter: a false alarm costs one more request to the model, or a line under the answer.
func promisesWork(answer string) bool {
	answer = strings.TrimSpace(answer)
	if answer == "" || utf8.RuneCountInString(answer) > maxStallAnswerRunes {
		return false
	}
	if workInProgress.MatchString(answer) {
		return true
	}
	// "Ще започна веднага. Искаш ли да продължа?" hands the turn back to the user.
	if strings.HasSuffix(answer, "?") {
		return false
	}
	for _, at := range workStartsNow.FindAllStringIndex(answer, -1) {
		if !afterCondition.MatchString(answer[at[1]:]) {
			return true
		}
	}
	return false
}

// asksForStatus tells whether a message asks how far some work is.
func asksForStatus(message string) bool { return statusQuestion.MatchString(message) }

// asksToSave tells whether a message asks to keep something: "запиши цялата статия".
func asksToSave(message string) bool { return saveVerb.MatchString(message) }

// asksToWrite tells whether a message asks for a change of a file: to keep something, or to
// add to or make a file it names.
func asksToWrite(message string) bool {
	return asksToSave(message) || (fileMention.MatchString(message) && (appendVerb.MatchString(message) || createVerb.MatchString(message)))
}

// leavesTheWork tells whether an answer to a request to write, given after no tool was
// called, does neither of the honest things: asks the user for what it needs, or says it
// cannot. What is left is talk about what the assistant will do. The real answers of
// 2026-11-20, 8 replays of one turn (one with a call): "ще презапиша файла…", "ще започна
// работа по … веднага", "ще обновя файла…", "мога да започна с първия блок веднага" - six
// wordings of one promise, so the promise itself is not looked for, its absence of a
// question is.
func leavesTheWork(answer string) bool {
	answer = strings.TrimSpace(answer)
	if answer == "" || utf8.RuneCountInString(answer) > maxStallAnswerRunes {
		return false
	}
	return !strings.Contains(answer, "?") && !refusal.MatchString(answer) && !asksTheUser.MatchString(answer)
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

// generateGuarded runs the rounds (generateRounds, with the retries of generateChecked), puts
// under the answer the line of the first check that finds it wrong (answerChecks) and, when links
// were not added to the library in the turn, tells the person which. Which model runs them is
// decided by generateResponse (model_routing.go), which calls this.
func (o *Orchestrator) generateGuarded(
	ctx context.Context,
	finalMessages []openai.ChatCompletionMessage,
	tools []openai.Tool,
	onDelta deltaFunc,
) (string, []openai.ToolCall, error) {
	content, executed, err := o.generateChecked(ctx, finalMessages, tools, onDelta)
	if err != nil {
		return content, executed, err
	}
	userMessage := lastUserContent(finalMessages)
	note := o.checkAnswer(ctx, answerFacts{
		content: content, executed: executed, turn: turnFrom(ctx),
		userMessage: userMessage, userNamedFile: fileMention.MatchString(userMessage),
	})
	// The model was told which links were refused, and in the real runs it wrote that all of
	// them were added over a link it had changed (plan item P53): the line is not left to it.
	report := turnFrom(ctx).libraryReport()
	for _, line := range []string{note, report} {
		if line == "" {
			continue
		}
		content += "\n\n" + line
		if onDelta != nil {
			if err := onDelta("\n\n"+line, ""); err != nil {
				return content, executed, err
			}
		}
	}
	return content, executed, nil
}

// generateChecked runs the rounds and, when the answer claims a write or promises work that no
// tool did, asks the turn once more with the file tools open (see the comment at the top of this
// file). The checks of the answer that is kept are in answerChecks.
func (o *Orchestrator) generateChecked(
	ctx context.Context,
	finalMessages []openai.ChatCompletionMessage,
	tools []openai.Tool,
	onDelta deltaFunc,
) (string, []openai.ToolCall, error) {
	if !o.hasFileGroup() {
		return o.generateRounds(ctx, finalMessages, tools, onDelta)
	}

	messages := finalMessages
	if asksToRemoveFile(lastUserContent(finalMessages)) {
		metrics.UserFileOps.WithLabelValues("removal", "note").Inc()
		messages = withNoteOnLastUser(finalMessages, removalNote)
	}

	content, executed, err := o.generateRounds(ctx, messages, tools, onDelta)
	if err != nil {
		return content, executed, err
	}
	turn := turnFrom(ctx)
	userMessage := lastUserContent(finalMessages)
	userNamedFile := fileMention.MatchString(userMessage)
	// stalled: the answer promises work for later or says it is going on, and no tool of
	// any kind was called in the turn. ignored: the message asked for a change of a file, no
	// tool of any kind was called, and the answer neither asks nor refuses (leavesTheWork).
	stalled := len(executed) == 0 && promisesWork(content)
	ignored := len(executed) == 0 && !asksForStatus(userMessage) && asksToWrite(userMessage) && leavesTheWork(content)

	var retry *fileRetry
	switch {
	case unbackedFileClaim(content, turn.filesWritten() > 0, userNamedFile && len(executed) == 0) == writeCorrection && !usedFileTools(executed):
		retry = &writeRetry
	case ignored, stalled && !asksForStatus(userMessage) &&
		(userNamedFile || fileMention.MatchString(content) || asksToSave(userMessage)):
		retry = &stallRetry
	}
	if retry == nil {
		return content, executed, nil
	}
	open, required := o.retryFileTools(tools, turn)
	if len(required) == 0 {
		return content, executed, nil
	}
	// "Add to the file" after a history in which the model said it had deleted it: left a
	// choice the model took write_file (a replacement, which waits for the user's
	// confirmation) instead of append_file. The first round of the retry offers no
	// write_file; the later rounds still do, for a file that is not there.
	if asksToAppend(userMessage) {
		required = withoutTools(required, writeFileTool)
	}
	// The retry is for the text the answer said it wrote: saving a page is not what it claimed.
	required = withoutTools(required, savePageTool)
	retried, retriedCalls, err := o.retryUnbacked(ctx, messages, open, required, *retry, onDelta)
	return retried, append(executed, retriedCalls...), err
}

// fileRetry is what differs between the two reasons to ask a turn again with the file tools:
// the note for the model only, the line the client sees, and the label of the counter.
type fileRetry struct {
	note, notice, op string
}

var (
	// writeRetry: the answer claimed a write that no tool made (P52).
	writeRetry = fileRetry{note: retryNote, notice: retryNotice, op: "claim"}
	// stallRetry: the answer promised the work for later and no tool was called (P53).
	stallRetry = fileRetry{note: stallNote, notice: stallNotice, op: "stall"}
)

// usedFileTools tells whether the turn called any tool of the file group, the stub
// included. Such a turn may have made a proposal or read a file; its claim is only
// corrected, not retried.
func usedFileTools(calls []openai.ToolCall) bool {
	return slices.ContainsFunc(calls, func(c openai.ToolCall) bool {
		switch c.Function.Name {
		case filesStubTool, listFilesTool, readFileTool, writeFileTool, appendFileTool, savePageTool:
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

// retryUnbacked asks the model again, when its answer claimed a file was written and
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
//
// The same is done for an answer that promised the work for later (fileRetry, P53).
func (o *Orchestrator) retryUnbacked(
	ctx context.Context,
	messages []openai.ChatCompletionMessage,
	open, required []openai.Tool,
	why fileRetry,
	onDelta deltaFunc,
) (string, []openai.ToolCall, error) {
	slog.WarnContext(ctx, "the answer claims or promises a file change that no tool made; the turn is asked again with the file tools",
		"reason", why.op)
	metrics.UserFileOps.WithLabelValues(why.op, "retried").Inc()
	if onDelta != nil {
		if err := onDelta(why.notice, ""); err != nil {
			return "", nil, err
		}
	}
	return o.runRounds(ctx, withNoteOnLastUser(messages, why.note), open, onDelta, required)
}
