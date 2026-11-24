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
// it; "платих …" marks a kept bill paid. The code answers all of it. Off unless BILLS_ENABLED.

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
