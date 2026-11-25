package agent

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/metrics"
)

// The checks of a finished answer: the line the server puts under an answer that says what the
// tools of its turn did not do. They are one ordered list. Each looks at the same facts of the
// turn, and the first one that finds the answer wrong puts its line under it; the rest are not
// asked, so an answer gets at most one "⚠️", the one that matters most coming first. A check is
// added, measured or taken out on its own.
//
// Before the checks the turn may have been asked again (generateChecked: an answer that claimed a
// write or promised work, with no tool called, is retried with the file tools open); the checks
// see the answer that is kept. After them comes the list of the links that were not added to the
// library (libraryReport), which is a report, not a correction, and is there whatever they say.

// answerFacts is what the checks of an answer look at.
type answerFacts struct {
	content  string
	executed []openai.ToolCall
	// userMessage is the person's last message, as written (without the notes for the model).
	userMessage string
	// userNamedFile: the message names a file.
	userNamedFile bool
	turn          *turnState
}

// answerCheck is one check of a finished answer.
type answerCheck struct {
	name string
	// fileTools: a check of what is said about the user's files; it runs only when the file
	// tools are on (with them off there is nothing such an answer could have done).
	fileTools bool
	// note is the line under an answer the check finds wrong, "" when it passes.
	note func(ctx context.Context, o *Orchestrator, f answerFacts) string
	// logMsg, op and result are what is logged and counted (user_files_operations_total) when
	// the check puts its line under an answer.
	logMsg     string
	op, result string
}

// answerChecks are the checks, in the order they are asked.
var answerChecks = []answerCheck{
	{
		// "Записах го" / "изтрих го" and no file tool wrote (see files_guard.go).
		name: "claim", fileTools: true, op: "claim", result: "corrected",
		logMsg: "the answer claims a change of the files that did not happen; a correction is added",
		note: func(_ context.Context, _ *Orchestrator, f answerFacts) string {
			return unbackedFileClaim(f.content, f.turn.filesWritten() > 0, f.userNamedFile && len(f.executed) == 0)
		},
	},
	{
		// Work promised for later or said to be going on, and no tool of any kind called (P53).
		name: "stall", fileTools: true, op: "stall", result: "corrected",
		logMsg: "the answer promises work for later and no tool was called; a correction is added",
		note: func(_ context.Context, _ *Orchestrator, f answerFacts) string {
			// While the server walks a task of the conversation, work does go on (task_run.go).
			if len(f.executed) == 0 && promisesWork(f.content) && !f.turn.taskRunningNow() {
				return stallCorrection
			}
			return ""
		},
	},
	{
		// A request to change a file, no tool called, and an answer that neither asks nor
		// refuses: the retry could not be made, or made no call. Nothing was written.
		name: "ignored", fileTools: true, op: "stall", result: "corrected",
		logMsg: "the message asked for a file change and the answer did nothing about it; a correction is added",
		note: func(_ context.Context, _ *Orchestrator, f answerFacts) string {
			if len(f.executed) == 0 && !asksForStatus(f.userMessage) && asksToWrite(f.userMessage) && leavesTheWork(f.content) {
				return writeCorrection
			}
			return ""
		},
	},
	{
		// "Успя ли?" - "Да, успях", and no file was changed since the person's earlier messages (P54).
		name: "status", fileTools: true, op: "status", result: "corrected",
		logMsg: "the answer to a question about the work says it is done and no file was changed; a correction is added",
		note: func(ctx context.Context, o *Orchestrator, f answerFacts) string {
			if o.statusClaimUnbacked(ctx, f.turn, f.userMessage, f.content, len(f.executed) > 0) {
				return statusCorrection
			}
			return ""
		},
	},
	{
		// "Търсих в библиотеката…" with no tool called, where the turn could not be asked again
		// with the tools of that place (lookup_guard.go).
		name: "lookup", op: "lookup", result: "corrected",
		logMsg: "the answer says it looked somewhere and no tool was called; a correction is added",
		note: func(_ context.Context, _ *Orchestrator, f answerFacts) string {
			if len(f.executed) == 0 && lookupClaim(f.content, f.userMessage) != "" {
				return lookupCorrection
			}
			return ""
		},
	},
	{
		// "Записах в базата", "ще те напомня седмица преди": the model keeps no bill and sets no
		// reminder, the code does (bill_claim.go).
		name: "bill_claim", op: "bill_claim", result: "corrected",
		logMsg: "the answer says a bill or a reminder was kept, and the model keeps neither; a correction is added",
		note: func(_ context.Context, _ *Orchestrator, f answerFacts) string {
			if claimsABillOrReminder(f.content) {
				return billClaimCorrection
			}
			return ""
		},
	},
	{
		// An "extraction" of a source no tool opened (extraction_claim.go). Not under an answer
		// that has a "⚠️" of its own already.
		name: "extraction", op: "extraction", result: "noted",
		logMsg: "the answer delivers an extraction of a source and the turn opened none; a note is added",
		note: func(_ context.Context, _ *Orchestrator, f answerFacts) string {
			return unbackedExtraction(f.content, f.executed, strings.Contains(f.content, "⚠️"))
		},
	},
}

// checkAnswer is the line of the first check that finds the answer wrong, or "" when none does.
func (o *Orchestrator) checkAnswer(ctx context.Context, f answerFacts) string {
	fileTools := o.hasFileGroup()
	for _, c := range answerChecks {
		if c.fileTools && !fileTools {
			continue
		}
		if note := c.note(ctx, o, f); note != "" {
			slog.WarnContext(ctx, c.logMsg, "check", c.name,
				"answer_runes", utf8.RuneCountInString(f.content), "tools_called", len(f.executed))
			metrics.UserFileOps.WithLabelValues(c.op, c.result).Inc()
			return note
		}
	}
	return ""
}
