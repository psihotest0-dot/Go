package agent

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"go-monolith/internal/bills"
	"go-monolith/metrics"
)

// Bills (the owner, 2026-11-24): an uploaded bill is read by the server (bills.Service.Read, from
// POST /v1/bills or a document sent to the Telegram bot) and said back with the question whether to
// keep it. The person's "да" in the chat keeps it, with the reminder of its due day; "не" drops
// it; "платих …" marks a kept bill paid; a bill with no document that comes every month or year
// ("запиши, че всеки месец на 5-ти плащам 20 € на …") is a standing bill, and "къде са сметките?"
// is answered with the database they are in. The code answers all of it. Off unless BILLS_ENABLED.

// billPaidWords: "платих тока", "преведох на зъболекаря", "платена е".
var billPaidWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:платих|изплатих|преведох|платена\s+е|платено\s+е|i\s+paid)(?:[^\p{L}]|$)`)

// billPaidMaxRunes: "платих сметката за тока днес" is short; a longer message tells more.
const billPaidMaxRunes = 100

// WithBills turns on the answers of the code about bills.
func (o *Orchestrator) WithBills(s *bills.Service) *Orchestrator {
	o.bills = s
	return o
}

// billAnswer is the answer of the code to a message about a bill, "" when the model answers it.
func (o *Orchestrator) billAnswer(ctx context.Context, userID uuid.UUID, session, message string) string {
	if o.bills == nil {
		return ""
	}
	message = strings.TrimSpace(withoutClosingNote(message))
	proposed, err := o.bills.Proposed(ctx, userID, session)
	if err != nil {
		slog.WarnContext(ctx, "could not read the proposed bill; the model answers", "err", err)
		return ""
	}
	if proposed != nil {
		switch {
		case isYes(message):
			return o.bills.Keep(ctx, userID, session, *proposed)
		case isNo(message):
			_ = o.bills.Drop(ctx, userID, session)
			metrics.Bills.WithLabelValues("declined").Inc()
			return "Добре, не записвам сметката."
		default:
			// Something else: the bill is not left to be kept by a later "да".
			_ = o.bills.Drop(ctx, userID, session)
			metrics.Bills.WithLabelValues("declined").Inc()
		}
	}
	// A standing bill ("всеки месец на 28-ми … 12 €", bills/standing.go) that waits for its yes.
	standing, err := o.bills.ProposedStanding(ctx, userID, session)
	if err != nil {
		slog.WarnContext(ctx, "could not read the proposed standing bill; the model answers", "err", err)
		return ""
	}
	if standing != nil {
		switch {
		case isYes(message):
			return o.bills.KeepStanding(ctx, userID, session, *standing)
		case isNo(message):
			_ = o.bills.DropStanding(ctx, userID, session)
			metrics.Bills.WithLabelValues("standing_declined").Inc()
			return "Добре, не записвам постоянната сметка."
		default:
			_ = o.bills.DropStanding(ctx, userID, session)
			metrics.Bills.WithLabelValues("standing_declined").Inc()
		}
	}
	// "Махни постоянната сметка за …", then a new one: the code reads them, not the model, which
	// on 2026-11-25 wrote such a bill into a text file of its own.
	if text, handled := o.bills.RemoveStanding(ctx, userID, message); handled {
		return text
	}
	if text, handled := o.bills.ProposeStanding(ctx, userID, session, message); handled {
		return text
	}
	// A question about the bills (a sum, the unpaid ones, a comparison) before "платих": "колко
	// платих за ток тази година?" is a question, "платих тока" is not.
	if text, handled := o.bills.Answer(ctx, userID, message); handled {
		return text
	}
	if utf8.RuneCountInString(message) <= billPaidMaxRunes && billPaidWords.MatchString(message) {
		if text, handled := o.bills.MarkPaid(ctx, userID, message); handled {
			return text
		}
	}
	return ""
}

// Bills is the service of the bills, nil when they are off (the Telegram bot reads the documents
// sent to it with it).
func (o *Orchestrator) Bills() *bills.Service { return o.bills }
