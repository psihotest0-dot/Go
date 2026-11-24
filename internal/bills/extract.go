// Package bills reads a bill - a PDF from an e-mail or a scanned paper one - into a record: who
// issued it, what it is for, the amount, the dates and how it is paid (the owner, 2026-11-24:
// "качвам сметка, прочита я и записва ... кога трябва да се плати", and a sum of all of them, "нещо
// като финансов съветник или счетоводител").
//
// The model only reads the text into fields (Extract). The code keeps what it can check: the
// amount and every date must be written in the text of the document itself (Verify), so a number
// the model made up never becomes a bill or a sum. The sums are the database's, not the model's.
package bills

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/llm"
)

// The kinds of a bill, the way the sums group them.
var Kinds = []string{"phone_internet", "electricity", "gas", "water", "heating", "rent", "insurance",
	"doctor", "dentist", "pharmacy", "car", "tax", "subscription", "shopping", "other"}

// KindNames are the kinds in Bulgarian.
var KindNames = map[string]string{
	"phone_internet": "телефон и интернет", "electricity": "ток", "gas": "газ", "water": "вода", "heating": "отопление",
	"rent": "наем", "insurance": "застраховки", "doctor": "лекар", "dentist": "зъболекар", "pharmacy": "аптека",
	"car": "кола", "tax": "данъци и такси", "subscription": "абонаменти", "shopping": "покупки", "other": "други",
}

// How a bill is paid.
const (
	DirectDebit = "direct_debit" // the issuer takes it from the account (Lastschrift)
	Transfer    = "transfer"     // the person must pay it (Überweisung)
	Paid        = "paid"         // already paid (a receipt)
	Unknown     = "unknown"
)

// Payments are the ways in the order the model is offered them.
var Payments = []string{DirectDebit, Transfer, Paid, Unknown}

// Repeats of a bill.
var Repeats = []string{"monthly", "quarterly", "yearly", "once", "unknown"}

const (
	// maxTextRunes is how much of a document the model reads: the amount, the dates and the way of
	// paying are on the first page of every bill measured (the longest, 3 pages, had them in its
	// first 2 000 characters).
	maxTextRunes  = 9000
	extractTimout = 3 * time.Minute
)

// Fields are what the model reads in a bill.
type Fields struct {
	IsBill      bool   `json:"is_bill"`
	Issuer      string `json:"issuer"`
	Kind        string `json:"kind"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	InvoiceDate string `json:"invoice_date"`
	DueDate     string `json:"due_date"`
	PaidDate    string `json:"paid_date"`
	Payment     string `json:"payment"`
	Repeat      string `json:"repeat"`
	Summary     string `json:"summary"`
}

const extractPrompt = `You read the text of a document and say whether it is a bill (an invoice, a receipt, a premium notice) and, if it is, its fields. The text may be German, Bulgarian or English, and it may come from a scan, with misread letters. Answer with the JSON only.

Fields:
- is_bill: true for an invoice, a receipt or a notice of a sum to pay or paid; false for anything else.
- issuer: the company or the person who issued it, short, as the bill names it ("Beispiel Mobilfunk", "Muster Versicherung", "Praxis Dr. X").
- kind: one of the kinds.
- amount: the total to pay (or paid), copied exactly as it is written in the text, digits and comma: "47,18". When the text has a "Rechnungsbetrag" and a "Zu zahlender Betrag", take the one to pay.
- currency: "EUR" for € or EUR.
- invoice_date: the date of the bill, copied exactly as written ("09.10.2026").
- due_date: the day the sum is due or will be taken from the account ("Fällig am", "zum Fälligkeitstag", "bis zum"), copied exactly as written; "" when there is none.
- paid_date: when the text says the sum was already paid or received ("dankend erhalten"), the day of it, copied exactly; else "".
- payment: "direct_debit" when the issuer takes it from the account (Lastschrift, "ziehen wir ... ein", "wird ... abgebucht"); "transfer" when the person must pay it ("Bitte überweisen", "begleichen Sie"); "paid" when it was already paid; "unknown".
- repeat: "monthly" (a phone, internet, electricity bill for a month), "quarterly", "yearly" ("jährlich", an insurance premium for a year), "once" (a doctor, a purchase), "unknown".
- summary: one short line in Bulgarian: what the bill is for ("Мобилен телефон, октомври 2026").

The text of the document:
`

func schema() json.RawMessage {
	str := func(n int) map[string]any { return map[string]any{"type": "string", "maxLength": n} }
	raw, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"is_bill":      map[string]any{"type": "boolean"},
			"issuer":       str(80),
			"kind":         map[string]any{"type": "string", "enum": Kinds},
			"amount":       str(20),
			"currency":     str(5),
			"invoice_date": str(20),
			"due_date":     str(20),
			"paid_date":    str(20),
			"payment":      map[string]any{"type": "string", "enum": Payments},
			"repeat":       map[string]any{"type": "string", "enum": Repeats},
			"summary":      str(120),
		},
		"required":             []string{"is_bill", "issuer", "kind", "amount", "currency", "invoice_date", "due_date", "paid_date", "payment", "repeat", "summary"},
		"additionalProperties": false,
	})
	return raw
}

// Extractor asks the model for the fields of a bill.
type Extractor struct{ llm llm.LLMClient }

// NewExtractor returns an extractor that asks client (the local model: a bill holds an address
// and an account, and is not sent to the paid one).
func NewExtractor(client llm.LLMClient) *Extractor { return &Extractor{llm: client} }

// Extract reads the fields of the document's text.
func (e *Extractor) Extract(ctx context.Context, text string) (Fields, error) {
	if utf8.RuneCountInString(text) > maxTextRunes {
		text = string([]rune(text)[:maxTextRunes])
	}
	reqCtx, cancel := context.WithTimeout(ctx, extractTimout)
	defer cancel()
	resp, err := e.llm.ChatWithJSONSchema(reqCtx, openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You read bills into fields. Answer with the JSON only."},
			{Role: openai.ChatMessageRoleUser, Content: extractPrompt + text},
		},
		//lint:ignore SA1019 llama-server reads max_tokens; max_completion_tokens is OpenAI's own name.
		MaxTokens:   400,
		Temperature: 0.1,
	}, schema())
	if err != nil {
		return Fields{}, err
	}
	if len(resp.Choices) == 0 {
		return Fields{}, fmt.Errorf("the model gave no answer")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	content = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(content, "```json"), "```"), "```")
	var f Fields
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &f); err != nil {
		return Fields{}, fmt.Errorf("the answer is not the JSON asked for (finish reason %q, %d characters): %w",
			resp.Choices[0].FinishReason, len(content), err)
	}
	return f, nil
}

// Bill is a bill the code has checked against its document.
type Bill struct {
	// ID and FileName are set for a bill that is stored.
	ID          int64
	FileName    string
	Issuer      string
	Kind        string
	Cents       int64
	Currency    string
	InvoiceDate *time.Time
	DueDate     *time.Time
	PaidDate    *time.Time
	Payment     string
	Repeat      string
	Summary     string
	// Dropped names what the model gave and the text does not hold: "due_date", "paid_date",
	// "invoice_date". The amount is never dropped: without it there is no bill.
	Dropped []string
}

// Problems of a document.
const (
	NotABill      = "not_a_bill"
	AmountMissing = "amount_missing"
)

var (
	amountShape = regexp.MustCompile(`^\d{1,3}(?:[.\s]?\d{3})*(?:,\d{2})?$|^\d+(?:\.\d{2})?$`)
	digitsOnly  = regexp.MustCompile(`[^\d]`)
)

// Verify keeps of the model's fields what the text of the document holds; problem is "" when the
// bill is good.
func Verify(f Fields, text string) (b Bill, problem string) {
	if !f.IsBill {
		return Bill{}, NotABill
	}
	cents, ok := parseAmount(f.Amount)
	if !ok || cents <= 0 || !amountInText(f.Amount, text) {
		return Bill{}, AmountMissing
	}
	b = Bill{
		Issuer: clean(f.Issuer, 80), Kind: f.Kind, Cents: cents, Currency: strings.ToUpper(strings.TrimSpace(f.Currency)),
		Payment: f.Payment, Repeat: f.Repeat, Summary: clean(f.Summary, 120),
	}
	if b.Currency == "" || b.Currency == "€" {
		b.Currency = "EUR"
	}
	if !contains(Kinds, b.Kind) {
		b.Kind = "other"
	}
	if !contains(Payments, b.Payment) {
		b.Payment = Unknown
	}
	if !contains(Repeats, b.Repeat) {
		b.Repeat = "unknown"
	}
	date := func(field, value string) *time.Time {
		if strings.TrimSpace(value) == "" {
			return nil
		}
		d, ok := parseDate(value)
		if !ok || !dateInText(d, text) {
			b.Dropped = append(b.Dropped, field)
			return nil
		}
		return &d
	}
	b.InvoiceDate = date("invoice_date", f.InvoiceDate)
	b.DueDate = date("due_date", f.DueDate)
	b.PaidDate = date("paid_date", f.PaidDate)
	if b.PaidDate != nil {
		b.Payment = Paid
	}
	return b, ""
}

func clean(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// parseAmount reads "47,18", "1.234,50", "23.90", "85" into cents.
func parseAmount(s string) (int64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "€"), "EUR"))
	s = strings.TrimSpace(s)
	if !amountShape.MatchString(s) {
		return 0, false
	}
	var whole, frac string
	if i := strings.LastIndexAny(s, ",."); i >= 0 && len(s)-i-1 == 2 {
		whole, frac = s[:i], s[i+1:]
	} else {
		whole, frac = s, "00"
	}
	whole = digitsOnly.ReplaceAllString(whole, "")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	return w*100 + f, true
}

// amountInText: the amount, as the model wrote it or with the other decimal mark, is in the text
// as a number of its own ("47,18" is not found in "147,18").
func amountInText(amount, text string) bool {
	a := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(amount), "€"), "EUR"))
	a = strings.TrimSpace(a)
	for _, form := range []string{a, strings.ReplaceAll(a, ",", "."), strings.ReplaceAll(a, ".", ",")} {
		re := regexp.MustCompile(`(?:^|[^\d.,])` + regexp.QuoteMeta(form) + `(?:[^\d]|[.,](?:[^\d]|$)|$)`)
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

var monthsDE = map[string]time.Month{"januar": 1, "februar": 2, "märz": 3, "maerz": 3, "april": 4, "mai": 5, "juni": 6,
	"juli": 7, "august": 8, "september": 9, "oktober": 10, "november": 11, "dezember": 12}

var (
	numericDate = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})\.(\d{2}|\d{4})$`)
	isoDate     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	wordDate    = regexp.MustCompile(`(?i)^(\d{1,2})\.?\s+(\p{L}+)\s+(\d{4})$`)
)

// parseDate reads "19.10.2026", "19.10.26", "2026-10-19", "7. Mai 2025".
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	var y, m, d int
	switch {
	case numericDate.MatchString(s):
		p := numericDate.FindStringSubmatch(s)
		d, _ = strconv.Atoi(p[1])
		m, _ = strconv.Atoi(p[2])
		y, _ = strconv.Atoi(p[3])
		if y < 100 {
			y += 2000
		}
	case isoDate.MatchString(s):
		p := isoDate.FindStringSubmatch(s)
		y, _ = strconv.Atoi(p[1])
		m, _ = strconv.Atoi(p[2])
		d, _ = strconv.Atoi(p[3])
	case wordDate.MatchString(s):
		p := wordDate.FindStringSubmatch(s)
		d, _ = strconv.Atoi(p[1])
		mm, ok := monthsDE[strings.ToLower(p[2])]
		if !ok {
			return time.Time{}, false
		}
		m = int(mm)
		y, _ = strconv.Atoi(p[3])
	default:
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d || int(t.Month()) != m || y < 2000 || y > 2100 {
		return time.Time{}, false
	}
	return t, true
}

// dateInText: the day is written in the text in one of the ways a German or Bulgarian bill writes it.
func dateInText(d time.Time, text string) bool {
	lower := strings.ToLower(text)
	forms := []string{
		d.Format("02.01.2006"), d.Format("2.1.2006"), d.Format("02.01.06"), d.Format("2.1.06"), d.Format("2006-01-02"),
	}
	for name, m := range monthsDE {
		if m == d.Month() {
			forms = append(forms, fmt.Sprintf("%d. %s %d", d.Day(), name, d.Year()), fmt.Sprintf("%d %s %d", d.Day(), name, d.Year()))
		}
	}
	for _, f := range forms {
		if strings.Contains(lower, strings.ToLower(f)) {
			return true
		}
	}
	return false
}
