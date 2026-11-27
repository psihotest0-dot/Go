package bills

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/db"
	"go-monolith/internal/reminders"
	"go-monolith/metrics"
)

// Standing bills (the owner, 2026-11-25): "запиши, че на всяка 28 дата имам сметка от … към
// …" is a bill with no document that comes every month (or year) on the same day. Asked in the chat,
// the model wrote it into a text file of its own, which the code never reads, and said the bills
// were "only in bills.txt". Now the code keeps it: the amount, the day and the way it is paid are
// read from the person's words by the code; the local model only names the issuer and the kind
// (one short request), and an issuer whose words are not in the message is not kept. It is said
// back and kept after "да", like a bill read from a document.
//
// A standing bill is not summed itself: a few days before each of its days (standingLeadDays) the
// code writes the payment of that day into the bills, with the reminder of the day before
// (StandingPass), so the sums, the lists, the monthly report and "платих …" see it as any other
// bill. A payment is not written for a month that already has a bill of the same kind and issuer
// from a document: the document is the bill of that month.

// standingLeadDays: the payment of a day is written this many days before it, so that the reminder
// of the day before has a bill to go with.
const standingLeadDays = 3

// standingFileName is the file name of the payments of standing bills, for the lists.
const standingFileName = "постоянна сметка"

// standingMaxRunes: a request for a standing bill is a sentence or two.
const standingMaxRunes = 300

// StandingIncomplete opens the answer of the code to a request that lacks the amount or the day; the
// next message may bring it ("14 евро", 2026-11-25, 17:55), see CompleteStanding.
const StandingIncomplete = "За постоянна сметка ми трябват "

// standingFollowMaxRunes: the missing piece alone is a few words.
const standingFollowMaxRunes = 60

// Standing is a standing bill.
type Standing struct {
	ID       int64
	Issuer   string
	Kind     string
	Cents    int64
	Currency string
	// Day is the day of the month (31: the last day of a shorter month); Month is 0 for a monthly
	// bill, the month of a yearly one.
	Day, Month int
	Payment    string
	Summary    string
	// RemindBefore is how many days before each day the reminder comes; -1 for none.
	RemindBefore int
	// Since is the first day a payment is written for: the day it was kept.
	Since reminders.Day
	// Until is the last day a payment may fall on ("до 01.05.2028 включително"), nil for none.
	Until *reminders.Day
}

var (
	// standingRepeatMonthly / standingRepeatYearly: the words of a bill that comes back.
	// "всяко първо число на месеца" (2026-11-25, 17:37) names the day in a word.
	standingRepeatMonthly = regexp.MustCompile(`(?i)(?:всеки\s+месец|всеки\s+\d{1,2}|всяк[оа]\s+\d{1,2}|всеки\s+\p{L}+\s+(?:ден|число)|всяк[оа]\s+\p{L}+\s+(?:число|дата)|на\s+месеца|ежемесечн|месечн|every\s+month|monthly|of\s+every\s+month)`)
	standingRepeatYearly  = regexp.MustCompile(`(?i)(?:всяка\s+година|ежегодн|годишн|every\s+year|yearly|annually)`)
	// standingBillWords: the message is about a bill or a payment.
	standingBillWords = regexp.MustCompile(`(?i)(?:сметк|плащам|плащане|плаща\s+се|абонамент|такс|вноск|наем|застраховк|изтегля|теглят|bill|subscription|i\s+pay)`)
	// standingSaveWords: the message asks to keep it, or says it is so.
	standingSaveWords = regexp.MustCompile(`(?i)(?:запиш|запише|запомн|добав|имам|плащам|плаща\s+се|изтегля|теглят|record|remember|add|i\s+have|i\s+pay)`)
	// standingQuestion: a question about the bills is not a request for a new one.
	standingQuestion = regexp.MustCompile(`(?i)^\s*(?:а\s+|и\s+)?(?:колко|кога|кои|какви|кой|коя|има\s+ли|how|when|which|what)(?:[^\p{L}]|$)`)

	// standingAmount: "16 евро", "41,20 €", "€16", "16.50 EUR", and with the thousands apart: "1.200 €",
	// "1 200 €", "1.200,50 €" (moneyNumber). Read it with readAmount.
	standingAmount = regexp.MustCompile(`(?i)(?:(` + moneyNumber + `)\s*(€|евро|eur|euro|лв\.?|лева|bgn)(?:[^\p{L}]|$)|(€)\s*(` + moneyNumber + `))`)
	// dayBeforeAmount: the words of a day just before a sum - "на 5 100 €" is the 5th and 100 €.
	dayBeforeAmount = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:на|в|всеки|всяка|всяко|до)\s+$`)
	// standingDayWords: "на 10-то число", "всяка 28 дата", "всеки 28-ми", "on the 5th".
	standingDayWords = regexp.MustCompile(`(?i)(?:(?:на|всеки|всяка|всяко|в)\s+(\d{1,2})(?:\s*-?\s*(?:ти|ви|ри|ми|то|во|ро|мо))?(?:\s+(?:число|дата|ден))?|(\d{1,2})\s*-?\s*(?:ти|ви|ри|ми|то|во|ро|мо)(?:\s+(?:число|дата|ден))?|(\d{1,2})\s+(?:число|дата)|on\s+the\s+(\d{1,2})(?:st|nd|rd|th)?)(?:[^\p{L}\d]|$)`)

	// The way it is paid, from the person's words only.
	standingDebitWords    = regexp.MustCompile(`(?i)(?:автоматичн|изтегля|теглят|директен\s+дебит|lastschrift|abbuchung|direct\s+debit|от\s+картата|от\s+сметката\s+ми)`)
	standingTransferWords = regexp.MustCompile(`(?i)(?:превеждам|превод|плащам\s+(?:го\s+|я\s+)?сам|по\s+банков|transfer)`)

	oneDecimal = regexp.MustCompile(`[.,]\d$`)

	// untilWords: the last day of a bill that comes back - "до 01.05.2028", "последното плащане е на
	// 01.05.2028", "until 1.7.2027".
	untilWords = regexp.MustCompile(`(?i)(?:(?:^|[^\p{L}])(?:до|until|till)|последн\p{L}*\s+(?:плащане|вноска)\s+(?:е\s+)?(?:на\s+)?)\s*(\d{1,2}\.\d{1,2}\.(?:\d{4}|\d{2}))`)

	// standingDayWord: the day as a word, "всяко първо число", "на десето".
	standingDayWord = regexp.MustCompile(`(?i)(?:на|всеки|всяка|всяко|в)\s+(\p{L}+)\s+(?:число|дата|ден)`)
)

// dayWords are the days of a month as words, the longer first ("петнадесето" is not "пето").
var dayWords = []struct {
	stem string
	day  int
}{
	{"двадесет и пе", 25}, {"двадесет и пър", 21}, {"тридесет и пър", 31}, {"последн", 31},
	{"петнадесет", 15}, {"двадесет", 20}, {"тридесет", 30}, {"десет", 10},
	{"първ", 1}, {"втор", 2}, {"трет", 3}, {"четвърт", 4}, {"пет", 5}, {"шест", 6}, {"седм", 7}, {"осм", 8}, {"девет", 9},
}

// untilDate is the last day a message names, nil for none.
func untilDate(m string) *time.Time {
	u := untilWords.FindStringSubmatch(m)
	if u == nil {
		return nil
	}
	t, ok := parseDate(u[1])
	if !ok {
		return nil
	}
	return &t
}

// dayOfWord is the day a word names, 0 for none.
func dayOfWord(w string) int {
	w = strings.ToLower(w)
	for _, d := range dayWords {
		if strings.HasPrefix(w, d.stem) {
			return d.day
		}
	}
	return 0
}

// keepProposals: a bill or a standing bill that was said back and never answered is forgotten after
// a week (once a day, DropStaleProposals).
const keepProposals = 7 * 24 * time.Hour

// RunStanding writes the payments of the standing bills until ctx ends (StandingPass), and once a
// day forgets the proposals nobody answered.
func (s *Service) RunStanding(ctx context.Context) {
	lastCleanup := time.Time{}
	for {
		if err := s.StandingPass(ctx); err != nil && ctx.Err() == nil {
			slog.Error("the payments of the standing bills could not be written", "error", err)
		}
		if now := s.now(); now.Sub(lastCleanup) > 24*time.Hour {
			lastCleanup = now
			s.DropStaleProposals(ctx, now)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reportEvery):
		}
	}
}

// DropStaleProposals forgets the bills and the standing bills said back more than keepProposals ago
// and never answered: a waiting proposal is kept by the next "да" of its conversation, and a week
// later that "да" (to a reminder the server wrote there, say) is for something else. Before
// 2026-11-25 the query for the bills was there and nothing called it.
func (s *Service) DropStaleProposals(ctx context.Context, now time.Time) {
	before := now.Add(-keepProposals)
	bills, err := s.q.DeleteStaleProposedBills(ctx, before)
	if err != nil {
		slog.WarnContext(ctx, "old proposed bills could not be removed", "error", err)
	}
	standing, err := s.q.DeleteStaleProposedStanding(ctx, before)
	if err != nil {
		slog.WarnContext(ctx, "old proposed standing bills could not be removed", "error", err)
	}
	if bills+standing > 0 {
		metrics.Bills.WithLabelValues("stale_dropped").Add(float64(bills + standing))
		slog.InfoContext(ctx, "old proposals of bills nobody answered were removed", "bills", bills, "standing", standing)
	}
}

// StandingRequest is a request for a standing bill as the code read it, before the model names the
// issuer. Missing names what the message lacks ("сумата", "деня") when it asks for one and is not
// complete.
type StandingRequest struct {
	Cents    int64
	Currency string
	Day      int
	Month    int
	Payment  string
	Missing  []string
	// Several is the number of amounts when the message names more than one bill.
	Several int
	// Until is the last day ("до 01.05.2028 включително"), nil for none.
	Until *time.Time
}

// ReadStandingRequest tells whether a message asks to keep a bill that comes back every month or
// year, and what of it the code reads from the words. ok is false when it is no such request.
func ReadStandingRequest(message string) (r StandingRequest, ok bool) {
	m := strings.TrimSpace(message)
	if m == "" || utf8.RuneCountInString(m) > standingMaxRunes || strings.HasSuffix(m, "?") || standingQuestion.MatchString(m) {
		return r, false
	}
	monthly, yearly := standingRepeatMonthly.MatchString(m), standingRepeatYearly.MatchString(m)
	if !(monthly || yearly) || !standingBillWords.MatchString(m) || !standingSaveWords.MatchString(m) {
		return r, false
	}
	// The last day (2026-11-25, 18:33: "… до 01.05.2028 включително, плащам … лизинг"); its digits
	// are not the day of the month.
	r.Until = untilDate(m)
	// The amount, and the place it takes, so that its digits are not read as the day.
	amountAt := []int{-1, -1}
	if cents, currency, at, okA := readAmount(m); okA {
		r.Cents, r.Currency = cents, currency
		amountAt = at[:]
	}
	for _, d := range standingDayWords.FindAllStringSubmatchIndex(standingWithoutDates(m), -1) {
		if d[0] < amountAt[1] && amountAt[0] < d[1] {
			continue // the digits of the amount
		}
		for g := 2; g+1 < len(d); g += 2 {
			if d[g] >= 0 {
				if n, err := strconv.Atoi(m[d[g]:d[g+1]]); err == nil && n >= 1 && n <= 31 {
					r.Day = n
				}
				break
			}
		}
		if r.Day > 0 {
			break
		}
	}
	if r.Day == 0 {
		if w := standingDayWord.FindStringSubmatch(m); w != nil {
			r.Day = dayOfWord(w[1])
		}
	}
	// "Наем: 500 € на 1-во … / Телефон: 40 € на 10-то …": several bills in one message. On 2026-11-25
	// (17:35) four of them became one, with the issuer of one and the amount of another.
	if n := len(standingAmount.FindAllStringIndex(m, -1)); n > 1 {
		r.Several = n
	}
	if yearly && !monthly {
		if mw := monthWord.FindStringSubmatch(m); mw != nil {
			r.Month = int(monthsBG[strings.ToLower(mw[1])])
		} else {
			r.Missing = append(r.Missing, "месеца")
		}
	}
	switch {
	case standingDebitWords.MatchString(m):
		r.Payment = DirectDebit
	case standingTransferWords.MatchString(m):
		r.Payment = Transfer
	default:
		r.Payment = Unknown
	}
	if r.Cents == 0 {
		r.Missing = append(r.Missing, "сумата")
	}
	if r.Day == 0 {
		r.Missing = append(r.Missing, "деня от месеца")
	}
	return r, true
}

// moneyNumber is a sum as people write it: "16", "41,20", "16.50", and with the thousands apart by a
// dot or a space ("1.200", "1 200", "1.200,50"). On 2026-11-25 "1.200 €" was read as 200 € - in the
// request and in the correction "сумата е 1.200 €" alike.
const moneyNumber = `\d{1,3}(?:[. \x{00A0}\x{202F}]\d{3})+(?:,\d{1,2})?|\d{1,6}(?:[.,]\d{1,2})?`

// spaceInNumber is a space that may stand between the thousands.
var spaceInNumber = strings.NewReplacer(" ", " ", " ", " ")

// readAmount is the sum a message names (standingAmount), its currency and the place it takes; ok is
// false when it names none. The thousands apart by a space after the words of a day are the day and
// the sum: "на 5 100 €" is 100 €.
func readAmount(m string) (cents int64, currency string, at [2]int, ok bool) {
	for from := 0; from < len(m); {
		a := standingAmount.FindStringSubmatchIndex(m[from:])
		if a == nil {
			return 0, "", at, false
		}
		for i := range a {
			if a[i] >= 0 {
				a[i] += from
			}
		}
		num, cur := a[2:4], a[4:6]
		if a[2] < 0 {
			num, cur = a[8:10], a[6:8]
		}
		amount := spaceInNumber.Replace(m[num[0]:num[1]])
		if sp := strings.IndexByte(amount, ' '); sp > 0 && dayBeforeAmount.MatchString(m[:num[0]]) {
			// Again from the digits after the first space (the digits before it are ASCII, so it
			// stands at the same byte in m; a no-break space is longer than one byte).
			_, size := utf8.DecodeRuneInString(m[num[0]+sp:])
			from = num[0] + sp + size
			continue
		}
		if oneDecimal.MatchString(amount) {
			amount += "0" // "12,5 евро"
		}
		c, okA := parseAmount(amount)
		if !okA || c <= 0 {
			return 0, "", at, false
		}
		return c, currencyOf(m[cur[0]:cur[1]]), [2]int{a[0], a[1]}, true
	}
	return 0, "", at, false
}

func currencyOf(s string) string {
	switch strings.ToLower(strings.TrimSuffix(s, ".")) {
	case "лв", "лева", "bgn":
		return "BGN"
	}
	return "EUR"
}

// StandingFields are what the model names in a request for a standing bill.
type StandingFields struct {
	Issuer  string `json:"issuer"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

const standingPrompt = `The person asks to keep a bill they pay again and again (every month or every year). Say who it is paid to and what it is for. Answer with the JSON only.

- issuer: the company or the person it is paid to, as the message names it - copy the words, do not correct or complete them; "" when the message names none.
- kind: one of the kinds ("subscription" for a subscription to a service or games, "rent" for a rent, "phone_internet" for a phone or internet).
- summary: one short line in Bulgarian: what it is for ("Абонамент за игри").

The message:
`

func standingSchema() json.RawMessage {
	str := func(n int) map[string]any { return map[string]any{"type": "string", "maxLength": n} }
	raw, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"issuer":  str(80),
			"kind":    map[string]any{"type": "string", "enum": Kinds},
			"summary": str(120),
		},
		"required":             []string{"issuer", "kind", "summary"},
		"additionalProperties": false,
	})
	return raw
}

// ExtractStanding asks the model who a standing bill is paid to and what it is for.
func (e *Extractor) ExtractStanding(ctx context.Context, message string) (StandingFields, error) {
	reqCtx, cancel := context.WithTimeout(ctx, extractTimout)
	defer cancel()
	resp, err := e.llm.ChatWithJSONSchema(reqCtx, openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You read a request about a bill into fields. Answer with the JSON only."},
			{Role: openai.ChatMessageRoleUser, Content: standingPrompt + message},
		},
		//lint:ignore SA1019 llama-server reads max_tokens; max_completion_tokens is OpenAI's own name.
		MaxTokens:   200,
		Temperature: 0.1,
	}, standingSchema())
	if err != nil {
		return StandingFields{}, err
	}
	if len(resp.Choices) == 0 {
		return StandingFields{}, fmt.Errorf("the model gave no answer")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	content = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(content, "```json"), "```"), "```")
	var f StandingFields
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &f); err != nil {
		return StandingFields{}, fmt.Errorf("the answer is not the JSON asked for: %w", err)
	}
	return f, nil
}

// standingIssuer keeps the issuer the model named only when one of its words is in the message:
// "Beispiel Spiele Club" for "beispeil spiele club" (the owner's typo) shares "spiele".
func standingIssuer(issuer, message string) string {
	issuer = clean(issuer, 80)
	if issuer == "" {
		return ""
	}
	in := stems(message)
	for st := range stems(issuer) {
		if in[st] {
			return issuer
		}
	}
	return ""
}

// standingKind is the kind the person's words name, else the model's, else "other".
func standingKind(model, message string) string {
	for _, k := range kindWords {
		if k.re.MatchString(message) {
			return k.kind
		}
	}
	if contains(Kinds, model) {
		return model
	}
	return "other"
}

// ProposeStanding answers a request for a standing bill: what the code read and the question
// whether to keep it. handled is false when the message asks for none (the model answers it).
func (s *Service) ProposeStanding(ctx context.Context, userID uuid.UUID, session, message string) (text string, handled bool) {
	r, ok := ReadStandingRequest(message)
	if !ok {
		return "", false
	}
	if r.Several > 1 {
		metrics.Bills.WithLabelValues("standing_several").Inc()
		return fmt.Sprintf("Виждам %d сметки в едно съобщение — записвам постоянните сметки по една, за да не ги смеся. "+
			"Прати ми ги поотделно, например: „запиши, че всеки месец на 1-во плащам 500 € наем, изтегля се автоматично“, после „да“, после следващата. "+
			"Нищо не е записано.", r.Several), true
	}
	if len(r.Missing) > 0 {
		metrics.Bills.WithLabelValues("standing_incomplete").Inc()
		return StandingIncomplete + strings.Join(r.Missing, " и ") + ". Например: „запиши, че всеки месец на 10-то плащам 40 € за телефон на …“ " +
			"или „запиши, че всяка година на 15 март плащам 120 € застраховка на …“. Нищо не е записано.", true
	}
	f, err := s.extractor.ExtractStanding(ctx, message)
	if err != nil {
		metrics.Bills.WithLabelValues("standing_extract_failed").Inc()
		slog.WarnContext(ctx, "the issuer of a standing bill could not be read", "err", err)
		return "Не успях да разчета на кого се плаща сметката. Опитай пак след малко. Нищо не е записано.", true
	}
	st := Standing{Issuer: standingIssuer(f.Issuer, message), Kind: standingKind(f.Kind, message), Cents: r.Cents, Currency: r.Currency,
		Day: r.Day, Month: r.Month, Payment: r.Payment, Summary: clean(f.Summary, 120), RemindBefore: 1}
	st.Until = dayPtr(r.Until)
	// "… изтегля се автоматично, без напомняне" in the request itself.
	if c, ok := ReadCorrection(message); ok && c.RemindBefore != nil {
		st.RemindBefore = *c.RemindBefore
	}

	tx, err := s.d.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко.", true
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if _, err := q.DropProposedBill(ctx, db.DropProposedBillParams{UserID: userID, SessionID: session}); err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко.", true
	}
	if _, err := q.DropProposedStanding(ctx, db.DropProposedStandingParams{UserID: userID, SessionID: session}); err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко.", true
	}
	task, err := dropWaitingTask(ctx, q, userID, session)
	if err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко.", true
	}
	row, err := q.ProposeStanding(ctx, db.ProposeStandingParams{UserID: userID, SessionID: session, Issuer: st.Issuer, Kind: st.Kind,
		AmountCents: st.Cents, Currency: st.Currency, Day: int32(st.Day), Month: int32(st.Month), Payment: st.Payment, Summary: st.Summary})
	if err != nil {
		slog.WarnContext(ctx, "a standing bill could not be proposed", "err", err)
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко.", true
	}
	if st.RemindBefore != 1 || st.Until != nil {
		if _, err := q.UpdateProposedStanding(ctx, db.UpdateProposedStandingParams{ID: row.ID, UserID: userID, AmountCents: st.Cents,
			Payment: st.Payment, RemindDaysBefore: int32(st.RemindBefore), Day: int32(st.Day), Month: int32(st.Month), Until: untilParam(st.Until)}); err != nil {
			return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко.", true
		}
	}
	if err := tx.Commit(); err != nil {
		return "Не успях да запазя сметката (грешка в базата). Опитай пак след малко.", true
	}
	countDroppedTask(task)
	if s.reminders != nil {
		_ = s.reminders.DropProposed(ctx, userID, session)
	}
	metrics.Bills.WithLabelValues("standing_proposed").Inc()
	today := reminders.DayOf(s.now(), s.loc)
	st.Since = today
	return standingProposalText(st, today, s.reminders != nil), true
}

// CompleteStanding answers the message after a request the code said was incomplete: earlier is
// that request, message the missing piece ("14 евро", "на 28-ми"). handled is false when the message
// is not a short piece the request lacked.
func (s *Service) CompleteStanding(ctx context.Context, userID uuid.UUID, session, earlier, message string) (text string, handled bool) {
	m := strings.TrimSpace(message)
	if m == "" || utf8.RuneCountInString(m) > standingFollowMaxRunes {
		return "", false
	}
	if !standingAmount.MatchString(m) && !standingDayWords.MatchString(m) && !standingDayWord.MatchString(m) {
		return "", false
	}
	return s.ProposeStanding(ctx, userID, session, strings.TrimSpace(earlier)+" "+m)
}

// ProposedStanding is the standing bill of the conversation that waits for the person's yes, or nil.
func (s *Service) ProposedStanding(ctx context.Context, userID uuid.UUID, session string) (*Standing, error) {
	row, err := s.q.GetProposedStanding(ctx, db.GetProposedStandingParams{UserID: userID, SessionID: session})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := standingFromRow(row, s.loc)
	return &st, nil
}

// DropStanding forgets the standing bill that waits in the conversation.
func (s *Service) DropStanding(ctx context.Context, userID uuid.UUID, session string) error {
	_, err := s.q.DropProposedStanding(ctx, db.DropProposedStandingParams{UserID: userID, SessionID: session})
	return err
}

// KeepStanding keeps the standing bill that waits in the conversation, writes the payments that
// are already due to be written, and says what was done.
func (s *Service) KeepStanding(ctx context.Context, userID uuid.UUID, session string, p Standing) string {
	row, err := s.q.KeepStanding(ctx, db.KeepStandingParams{ID: p.ID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return "Тази сметка вече е записана (или отказана)."
	}
	if err != nil {
		slog.WarnContext(ctx, "a standing bill could not be kept", "err", err)
		return "Не успях да запиша сметката (грешка в базата). Опитай пак след малко."
	}
	metrics.Bills.WithLabelValues("standing_kept").Inc()
	st := standingFromRow(row, s.loc)
	today := reminders.DayOf(s.now(), s.loc)
	text := "Записах постоянната сметка ✅ " + standingLine(st) + "."
	next := nextStandingDay(st, today)
	text += "\nСледващото плащане: " + reminders.DateText(next) + "."
	if s.reminders != nil {
		text += " " + capitalize(remindWhen(st.RemindBefore, "всяко")) + "."
	}
	text += "\nВлиза в сборовете и справките от " + reminders.DateText(next) + " нататък; махаш я с „махни постоянната сметка за …“."
	if err := s.standingPayments(ctx, row, today); err != nil {
		slog.WarnContext(ctx, "the payments of a standing bill could not be written", "err", err)
	}
	return text
}

func standingFromRow(r db.StandingBill, loc *time.Location) Standing {
	st := Standing{ID: r.ID, Issuer: r.Issuer, Kind: r.Kind, Cents: r.AmountCents, Currency: r.Currency, Day: int(r.Day), Month: int(r.Month),
		Payment: r.Payment, Summary: r.Summary, RemindBefore: int(r.RemindDaysBefore), Since: reminders.DayOf(r.CreatedAt, loc)}
	if r.Until.Valid {
		st.Until = dayPtr(&r.Until.Time)
	}
	return st
}

// standingDayIn is the day of a standing bill in a month (31 is the last day of a shorter one).
func standingDayIn(st Standing, year int, m time.Month) reminders.Day {
	return reminders.Day{Year: year, Month: m, Day: min(st.Day, daysInMonth(year, m))}
}

// nextStandingDay is the first day of the bill on or after from.
func nextStandingDay(st Standing, from reminders.Day) reminders.Day {
	if st.Month == 0 {
		d := standingDayIn(st, from.Year, from.Month)
		if d.Before(from) {
			first := reminders.Day{Year: from.Year, Month: from.Month, Day: 1}.AddDays(daysInMonth(from.Year, from.Month))
			d = standingDayIn(st, first.Year, first.Month)
		}
		return d
	}
	d := standingDayIn(st, from.Year, time.Month(st.Month))
	if d.Before(from) {
		d = standingDayIn(st, from.Year+1, time.Month(st.Month))
	}
	return d
}

// standingDays are the days of the bill from its first day up to to, and not after its last day,
// in order.
func standingDays(st Standing, to reminders.Day) []reminders.Day {
	if st.Until != nil && st.Until.Before(to) {
		to = *st.Until
	}
	var out []reminders.Day
	for d := nextStandingDay(st, st.Since); !to.Before(d); d = nextStandingDay(st, d.AddDays(1)) {
		out = append(out, d)
	}
	return out
}

// StandingPass writes the payments of the standing bills that are due to be written (see the top of
// the file). It is called with the monthly report, every few minutes.
func (s *Service) StandingPass(ctx context.Context) error {
	rows, err := s.q.AllKeptStanding(ctx)
	if err != nil {
		return err
	}
	today := reminders.DayOf(s.now(), s.loc)
	for _, r := range rows {
		if err := s.standingPayments(ctx, r, today); err != nil {
			return err
		}
	}
	return nil
}

// standingPayments writes the payments of one standing bill up to standingLeadDays from today.
func (s *Service) standingPayments(ctx context.Context, r db.StandingBill, today reminders.Day) error {
	st := standingFromRow(r, s.loc)
	for _, day := range standingDays(st, today.AddDays(standingLeadDays)) {
		month := monthPeriod(day.Year, day.Month)
		documented, err := s.documentedIn(ctx, r.UserID, st, month)
		if err != nil {
			return err
		}
		if documented {
			continue
		}
		due := day.UTC()
		repeat := "monthly"
		if st.Month != 0 {
			repeat = "yearly"
		}
		row, err := s.q.InsertStandingPayment(ctx, db.InsertStandingPaymentParams{
			UserID: r.UserID, SessionID: r.SessionID, Issuer: st.Issuer, Kind: st.Kind, AmountCents: st.Cents, Currency: st.Currency,
			DueDate: sql.NullTime{Time: due, Valid: true}, Payment: st.Payment, Repeat: repeat, Summary: st.Summary,
			FileName: standingFileName, FileHash: fmt.Sprintf("standing:%d:%04d-%02d", st.ID, day.Year, day.Month),
			StandingID: sql.NullInt64{Int64: st.ID, Valid: true}, RemindDaysBefore: int32(st.RemindBefore),
		})
		if errors.Is(err, sql.ErrNoRows) {
			continue // written already
		}
		if err != nil {
			return fmt.Errorf("write the payment of a standing bill: %w", err)
		}
		metrics.Bills.WithLabelValues("standing_payment").Inc()
		if s.reminders == nil {
			continue
		}
		plan := PlanReminder(fromRow(row), s.now(), s.loc)
		if plan.Text == "" {
			continue
		}
		rem, err := s.reminders.CreateAt(ctx, r.UserID, r.SessionID, plan.Text, plan.At, s.loc)
		if err != nil {
			slog.WarnContext(ctx, "the reminder of a standing bill could not be kept", "err", err)
			continue
		}
		if err := s.q.SetBillReminder(ctx, db.SetBillReminderParams{ID: row.ID, UserID: r.UserID, ReminderID: sql.NullInt64{Int64: rem.ID, Valid: true}}); err != nil {
			slog.WarnContext(ctx, "the reminder of a standing bill could not be tied to it", "err", err)
		}
		if s.wake != nil {
			s.wake()
		}
	}
	return nil
}

// documentedIn tells whether a month has a bill from a document of the same kind and issuer as the
// standing bill: then that document is the bill of the month.
func (s *Service) documentedIn(ctx context.Context, userID uuid.UUID, st Standing, month Period) (bool, error) {
	rows, err := s.q.KeptBillsBetween(ctx, db.KeptBillsBetweenParams{UserID: userID, FromDay: month.From.UTC(), ToDay: month.To.UTC()})
	if err != nil {
		return false, err
	}
	for _, b := range rows {
		if !b.StandingID.Valid && b.Kind == st.Kind && sameIssuer(b.Issuer, st.Issuer) {
			return true, nil
		}
	}
	return false, nil
}

// sameIssuer: two names share a word, or one of them is empty.
func sameIssuer(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	sa := stems(a)
	for st := range stems(b) {
		if sa[st] {
			return true
		}
	}
	return false
}

// replaceStandingPayments takes out the payments of standing bills of the month of a bill from a
// document of the same kind and issuer: the document is the bill of that month. It says what it
// took out, "" when nothing.
func (s *Service) replaceStandingPayments(ctx context.Context, userID uuid.UUID, b Bill) string {
	d := b.DueDate
	if d == nil {
		d = b.InvoiceDate
	}
	if d == nil {
		return ""
	}
	month := monthPeriod(d.Year(), d.Month())
	rows, err := s.q.StandingPaymentsOfKindBetween(ctx, db.StandingPaymentsOfKindBetweenParams{UserID: userID, Kind: b.Kind, FromDay: month.From.UTC(), ToDay: month.To.UTC()})
	if err != nil {
		slog.WarnContext(ctx, "could not look for the payments of standing bills", "err", err)
		return ""
	}
	var gone []string
	for _, r := range rows {
		if r.PaidDate.Valid || !sameIssuer(r.Issuer, b.Issuer) {
			continue
		}
		if n, err := s.q.DeleteBill(ctx, db.DeleteBillParams{ID: r.ID, UserID: userID}); err != nil || n == 0 {
			continue
		}
		if r.ReminderID.Valid && s.reminders != nil {
			_, _ = s.reminders.Cancel(ctx, userID, r.ReminderID.Int64)
		}
		gone = append(gone, oneLine(fromRow(r), s.loc))
	}
	if len(gone) == 0 {
		return ""
	}
	metrics.Bills.WithLabelValues("standing_replaced").Inc()
	return "Тази сметка заменя плащането от постоянната сметка за този месец (" + strings.Join(gone, "; ") + ") — то е махнато, за да не се брои два пъти."
}

// KeptStanding are the person's standing bills.
func (s *Service) KeptStanding(ctx context.Context, userID uuid.UUID) ([]Standing, error) {
	rows, err := s.q.ListKeptStanding(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Standing, 0, len(rows))
	for _, r := range rows {
		out = append(out, standingFromRow(r, s.loc))
	}
	return out, nil
}

var (
	// standingRemoveWords: "махни", "спри", "изтрий" … a standing bill, a subscription.
	standingRemoveWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:махни|махнете|премахни|изтрий|спри|отмени|прекрати|delete|remove|stop|cancel)(?:[^\p{L}]|$)`)
	standingRemoveWhat  = regexp.MustCompile(`(?i)(?:постоянн|абонамент|месечн\p{L}*\s+сметк|subscription|standing)`)
	reminderWord        = regexp.MustCompile(`(?i)(?:напомн|remind)`)
	// standingNumber: the number of the list, as the list says it ("махни постоянна 2", "махни
	// постоянната сметка 2", "махни абонамент №1"): right after the word of the list, or after "№" or
	// "номер". A number elsewhere is no number of the list ("абонамента за 5G", "от 28-ми").
	standingNumber = regexp.MustCompile(`(?i)(?:(?:постоянн\p{L}*|абонамент\p{L}*|standing|subscription)(?:\s+(?:сметк\p{L}*|bill))?\s+(?:№\s*|номер\s+)?|№\s*|номер\s+)(\d{1,2})(?:[^\p{L}\d-]|$)`)
)

// removeStems are the words of a request to remove that name no standing bill: "махни постоянната
// сметка", "спри абонамента" (the kind "абонаменти" shares its first letters with the request).
var removeStems = map[string]bool{
	"махни": true, "махне": true, "преми": true, "изтри": true, "спри": true, "спрет": true, "отмен": true, "прекр": true,
	"посто": true, "абона": true, "месеч": true, "ежеме": true, "плаща": true, "моля": true, "искам": true, "повеч": true, "нужда": true,
	"нужна": true, "remov": true, "delet": true, "stop": true, "cance": true, "the": true, "for": true, "stand": true, "subsc": true,
	"bill": true, "bills": true, "pleas": true,
}

// RemoveStanding answers "махни постоянната сметка за игри": the standing bill is removed with its
// payments still to come and their reminders; the payments of the days gone stay in the sums.
// handled is false when the message asks for no such thing or there is no standing bill.
func (s *Service) RemoveStanding(ctx context.Context, userID uuid.UUID, message string) (text string, handled bool) {
	m := strings.TrimSpace(message)
	// "Махни напомнянето за абонамента" is for the reminders (reminder_answer.go).
	if utf8.RuneCountInString(m) > standingMaxRunes || !standingRemoveWords.MatchString(m) || !standingRemoveWhat.MatchString(m) || reminderWord.MatchString(m) {
		return "", false
	}
	all, err := s.KeptStanding(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the standing bills", "err", err)
		return "", false
	}
	if len(all) == 0 {
		return "Нямаш записани постоянни сметки.", true
	}
	pick, named := pickStanding(all, m)
	if len(pick) != 1 {
		metrics.Bills.WithLabelValues("standing_remove_asked").Inc()
		var b strings.Builder
		if named && len(pick) == 0 {
			b.WriteString("Не намерих такава постоянна сметка — нищо не е махнато. Записаните са:\n")
		} else {
			b.WriteString("Коя постоянна сметка да махна?\n")
		}
		for i, st := range all {
			fmt.Fprintf(&b, "%d. %s\n", i+1, standingLine(st))
		}
		b.WriteString("Кажи „махни постоянна“ и номера, например „махни постоянна 1“.")
		return b.String(), true
	}
	st := all[pick[0]]
	today := reminders.DayOf(s.now(), s.loc)
	upcoming, err := s.q.UpcomingStandingPayments(ctx, db.UpcomingStandingPaymentsParams{UserID: userID, StandingID: sql.NullInt64{Int64: st.ID, Valid: true}, FromDay: today.UTC()})
	if err != nil {
		return "Не успях да махна постоянната сметка (грешка в базата). Опитай пак след малко.", true
	}
	for _, r := range upcoming {
		if n, err := s.q.DeleteBill(ctx, db.DeleteBillParams{ID: r.ID, UserID: userID}); err == nil && n > 0 && r.ReminderID.Valid && s.reminders != nil {
			_, _ = s.reminders.Cancel(ctx, userID, r.ReminderID.Int64)
		}
	}
	if n, err := s.q.DeleteStanding(ctx, db.DeleteStandingParams{ID: st.ID, UserID: userID}); err != nil || n == 0 {
		return "Не успях да махна постоянната сметка (грешка в базата). Опитай пак след малко.", true
	}
	metrics.Bills.WithLabelValues("standing_removed").Inc()
	text = "Махнах постоянната сметка ✅ " + standingLine(st) + "."
	if len(upcoming) > 0 {
		text += " Предстоящото плащане и напомнянето му са махнати."
	}
	return text + " Плащанията дотук остават в сметките.", true
}

// pickStanding are the indexes of the standing bills the message names - by its number in the list,
// by the words it shares with the issuer, the summary and the name of the kind, or by a word of the
// kind ("тока", "наема") - and whether the message names one at all. The words of the request itself
// name none (removeStems): on 2026-11-25 "спри абонамента за Netflix" took the only subscription
// (its kind is "абонаменти"), and with one standing bill any request to remove took that one.
func pickStanding(all []Standing, message string) (pick []int, named bool) {
	if m := standingNumber.FindStringSubmatch(message); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n >= 1 && n <= len(all) {
			return []int{n - 1}, true
		}
	}
	want := stems(message)
	for w := range want {
		if removeStems[w] {
			delete(want, w)
		}
	}
	// "Абонамент" is the word of the request, not the name of a kind.
	kinds := map[string]bool{}
	for _, k := range kindWords {
		if k.kind != "subscription" && k.re.MatchString(message) {
			kinds[k.kind] = true
		}
	}
	if len(want) == 0 && len(kinds) == 0 {
		return nil, false
	}
	best, bestScore := []int(nil), 0
	for i, st := range all {
		score := 0
		for w := range stems(st.Issuer + " " + st.Summary + " " + KindNames[st.Kind]) {
			if want[w] {
				score++
			}
		}
		if kinds[st.Kind] {
			score++
		}
		switch {
		case score == 0:
		case score > bestScore:
			best, bestScore = []int{i}, score
		case score == bestScore:
			best = append(best, i)
		}
	}
	return best, true
}

// standingWhen is "всеки месец на 28-мо число" / "всяка година на 15 март", with "до …" when it has
// a last day.
func standingWhen(st Standing) string {
	var s string
	switch {
	case st.Month != 0:
		s = fmt.Sprintf("всяка година на %d %s", st.Day, monthNamesBG[st.Month])
	case st.Day >= 29:
		s = fmt.Sprintf("всеки месец на %d-о число (в по-кратък месец — последния ден)", st.Day)
	default:
		s = fmt.Sprintf("всеки месец на %d-о число", st.Day)
	}
	if st.Until != nil {
		s += fmt.Sprintf(" до %02d.%02d.%d включително", st.Until.Day, int(st.Until.Month), st.Until.Year)
	}
	return s
}

// dayPtr is the day of a date, nil for nil.
func dayPtr(t *time.Time) *reminders.Day {
	if t == nil {
		return nil
	}
	d := reminders.Day{Year: t.Year(), Month: t.Month(), Day: t.Day()}
	return &d
}

// untilParam is the last day for the database.
func untilParam(d *reminders.Day) sql.NullTime {
	if d == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: d.UTC(), Valid: true}
}

// standingDates: dates in a message, which are no day of the month.
var standingDates = regexp.MustCompile(`\d{1,2}\.\d{1,2}\.(?:\d{4}|\d{2})`)

// standingWithoutDates is the message with its dates blanked out, as long as it was.
func standingWithoutDates(m string) string {
	return standingDates.ReplaceAllStringFunc(m, func(d string) string { return strings.Repeat("#", len(d)) })
}

// standingLine is "Beispiel Spiele Club (абонаменти), 14,00 € всеки месец на 28-о число".
func standingLine(st Standing) string {
	return title(Bill{Issuer: st.Issuer, Kind: st.Kind}) + ", " + Money(st.Cents, st.Currency) + " " + standingWhen(st)
}

var standingPaymentText = map[string]string{
	DirectDebit: "изтегля се автоматично",
	Transfer:    "плащаш я сам (превод)",
	Unknown:     "не каза как се плаща — ако се тегли автоматично, кажи го в молбата („… изтегля се автоматично“)",
}

func standingProposalText(st Standing, today reminders.Day, withReminders bool) string {
	var b strings.Builder
	b.WriteString("Постоянна сметка:\n")
	fmt.Fprintf(&b, "• %s", title(Bill{Issuer: st.Issuer, Kind: st.Kind}))
	if st.Summary != "" {
		fmt.Fprintf(&b, " — %s", st.Summary)
	}
	if st.Issuer == "" {
		b.WriteString(" (не разбрах на кого се плаща — ако искаш името, напиши го пак в молбата)")
	}
	fmt.Fprintf(&b, "\n• %s %s — %s\n", Money(st.Cents, st.Currency), standingWhen(st), standingPaymentText[st.Payment])
	next := nextStandingDay(st, today)
	fmt.Fprintf(&b, "• Първото плащане: %s", reminders.DateText(next))
	if withReminders {
		b.WriteString("; " + remindWhen(st.RemindBefore, "всяко"))
	}
	b.WriteString("\n\nДа я запиша ли в базата? Кажи „да“ или „не“.")
	return b.String()
}

// StandingLines are the lines of the standing bills for the answers about the bills, "" when none.
func (s *Service) StandingLines(ctx context.Context, userID uuid.UUID) string {
	all, err := s.KeptStanding(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "the standing bills could not be read for an answer about the bills", "err", err)
	}
	if err != nil || len(all) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Постоянни сметки:\n")
	for i, st := range all {
		fmt.Fprintf(&b, "%d. %s\n", i+1, standingLine(st))
	}
	return strings.TrimRight(b.String(), "\n")
}

// remindWhen is "напомням в 09:00 ден преди всяко" / "… 7 дни преди всяко" / "… в самия ден" / "без
// напомняне"; of names what the days are of ("всяко", "срока").
func remindWhen(days int, of string) string {
	switch {
	case days < 0:
		return "без напомняне"
	case days == 0:
		return "напомням в 09:00 в самия ден"
	case days == 1:
		return "напомням в 09:00 ден преди " + of
	}
	return fmt.Sprintf("напомням в 09:00 %d дни преди %s", days, of)
}
