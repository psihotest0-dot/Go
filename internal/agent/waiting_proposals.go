package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/bills"
	"go-monolith/internal/reminders"
	"go-monolith/internal/taskrun"
	"go-monolith/metrics"
)

// One question at a time (the code review of 2026-11-25). The code asks the person for a yes in
// three places: a task over many documents (task_run.go), a bill read from a document or a standing
// bill (bill_answer.go, and the uploads of bills.Service), and a reminder (reminder_answer.go). A
// "да" is for the question asked last:
//
//   - a new proposal drops the proposals of the other kinds that waited in the conversation (a bill
//     read from an upload drops a waiting task itself, in bills.Service);
//   - a task the server is walking answers a yes with "Вече работя по нея" only when nothing else
//     waits for one.
//
// Before this a running task answered every "да" with "Вече работя по нея", so a bill or a reminder
// of the same conversation could not be kept while it ran; and a task proposed before a bill was
// uploaded was started by the yes meant for the bill.

// The kinds of proposals, for dropOtherProposals.
const (
	proposalTask     = "task"
	proposalBill     = "bill" // a bill read from a document, or a standing bill
	proposalReminder = "reminder"
)

// proposalWaits tells whether something waits for the person's yes in the conversation: a proposed
// task, a bill, a standing bill or a reminder. A proposal that could not be read counts as waiting,
// so that the yes goes on to the answers that read it themselves.
func (o *Orchestrator) proposalWaits(ctx context.Context, userID uuid.UUID, session string) bool {
	waits := func(what string, found bool, err error) bool {
		if err != nil {
			slog.WarnContext(ctx, "could not read a proposal of the conversation", "proposal", what, "err", err)
			return true
		}
		return found
	}
	if o.runPlans != nil {
		p, err := o.runPlans.Proposed(ctx, userID, session)
		if waits(proposalTask, p != nil, err) {
			return true
		}
	}
	if o.bills != nil {
		b, err := o.bills.Proposed(ctx, userID, session)
		if waits(proposalBill, b != nil, err) {
			return true
		}
		st, err := o.bills.ProposedStanding(ctx, userID, session)
		if waits("standing_bill", st != nil, err) {
			return true
		}
	}
	if o.reminders != nil {
		r, err := o.reminders.store.Proposed(ctx, userID, session)
		if waits(proposalReminder, r != nil, err) {
			return true
		}
	}
	return false
}

// dropOtherProposals forgets the proposals of the conversation that are not of the kind keep: the
// person has just been asked a new question, and the "да" that follows is for it.
func (o *Orchestrator) dropOtherProposals(ctx context.Context, userID uuid.UUID, session, keep string) {
	dropped := func(what string, err error) bool {
		if err != nil {
			slog.WarnContext(ctx, "a proposal that waited could not be dropped", "proposal", what, "err", err)
			return false
		}
		slog.InfoContext(ctx, "a proposal that waited was dropped by a newer question", "proposal", what, "kept", keep)
		return true
	}
	if keep != proposalTask && o.runPlans != nil {
		if p, err := o.runPlans.Proposed(ctx, userID, session); err == nil && p != nil {
			if dropped(proposalTask, o.runPlans.DropProposed(ctx, userID, session)) {
				metrics.TaskRuns.WithLabelValues(p.Kind, "superseded").Inc()
			}
		}
	}
	if keep != proposalBill && o.bills != nil {
		if b, err := o.bills.Proposed(ctx, userID, session); err == nil && b != nil {
			if dropped(proposalBill, o.bills.Drop(ctx, userID, session)) {
				metrics.Bills.WithLabelValues("superseded").Inc()
			}
		}
		if st, err := o.bills.ProposedStanding(ctx, userID, session); err == nil && st != nil {
			if dropped("standing_bill", o.bills.DropStanding(ctx, userID, session)) {
				metrics.Bills.WithLabelValues("standing_superseded").Inc()
			}
		}
	}
	if keep != proposalReminder && o.reminders != nil {
		if r, err := o.reminders.store.Proposed(ctx, userID, session); err == nil && r != nil {
			if dropped(proposalReminder, o.reminders.store.DropProposed(ctx, userID, session)) {
				metrics.Reminders.WithLabelValues("superseded").Inc()
			}
		}
	}
}

// serverNoticeLogs start the records of the messages the server writes into a conversation on its
// own, outside a turn: a reminder that came, the monthly report of the bills, the report of a task.
var serverNoticeLogs = []string{reminders.SentLog, bills.ReportLog, taskrun.ReportLog}

// isServerNotice tells whether a stored record (messages.tool_log) is the one of a message the
// server wrote on its own (serverNoticeLogs).
func isServerNotice(toolLog string) bool {
	for _, log := range serverNoticeLogs {
		if strings.HasPrefix(toolLog, log) {
			return true
		}
	}
	return false
}

// answersServerNotice tells whether the person's message (the last of the history) follows a
// message the server wrote on its own (serverNoticeLogs): the history shows its record next to it.
func answersServerNotice(history []openai.ChatCompletionMessage) bool {
	n := len(history)
	if n < 2 || history[n-1].Role != openai.ChatMessageRoleUser || history[n-2].Role != openai.ChatMessageRoleAssistant {
		return false
	}
	for _, log := range serverNoticeLogs {
		if strings.Contains(history[n-2].Content, toolLogNoteHead+log) {
			return true
		}
	}
	return false
}

// yesAfterServerNotice is the answer of the code to a yes that follows a reminder or a report the
// server wrote into the conversation while a proposal waited from before it, "" otherwise. The
// "добре" to "⏰ Напомняне: …" kept the bill proposed before the reminder (the code review of
// 2026-11-25); a yes is for the question asked last, and a notice asks none. The proposal is kept,
// and the next yes, after this question, is for it.
func (o *Orchestrator) yesAfterServerNotice(ctx context.Context, userID uuid.UUID, session, message string, history []openai.ChatCompletionMessage) string {
	message = strings.TrimSpace(withoutClosingNote(message))
	if !isYes(message) || !answersServerNotice(history) || !o.proposalWaits(ctx, userID, session) {
		return ""
	}
	slog.InfoContext(ctx, "a yes after a notice of the server; the proposal before it is asked about again", "session_id", session)
	return fmt.Sprintf("„%s“ идва след известието ми по-горе (напомняне или справка), затова не го приемам за отговор на въпроса преди него. "+
		"Той още чака: кажи „да“, ако е за него, или „не“.", message)
}
