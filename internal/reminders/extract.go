package reminders

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/llm"
)

// MaxTextRunes bounds the text of a reminder (the column allows 300).
const MaxTextRunes = 200

// extractTimeout bounds the call of the model that reads a request.
const extractTimeout = 90 * time.Second

// Fields are what the model reads in a request for a reminder.
type Fields struct {
	IsReminder     bool   `json:"is_reminder"`
	Text           string `json:"text"`
	Date           string `json:"date"`
	Time           string `json:"time"`
	TimeGiven      bool   `json:"time_given"`
	MinutesFromNow int    `json:"minutes_from_now"`
	Repeat         string `json:"repeat"`
	DayBefore      bool   `json:"day_before"`
}

// Turn is a message of the conversation before the request (for a correction: "не, в 10").
type Turn struct {
	Role    string
	Content string
}

// Extractor asks the model for the fields of a request. It is a call of its own, with a short
// prompt that has the clock to the minute and the next days written out, not a round of the chat:
// the chat's prompt has the hour only (for its cache) and no table of the days.
type Extractor struct {
	llm llm.LLMClient
}

// NewExtractor returns an extractor that asks client.
func NewExtractor(client llm.LLMClient) *Extractor { return &Extractor{llm: client} }

const extractPrompt = `You read a person's request for a reminder and fill in its fields. Answer with the JSON only.

Now: %s (the person's clock, Europe/Berlin).
The days:
%s
Fields:
- is_reminder: true when the person asks to be reminded of something at a time, or tells a date of an event every year (a birthday, a name day, an anniversary) to be reminded of. false for "напомни ми какво говорихме" (tell me again what we said), for a question, and for anything else.
- text: what to remind of, in Bulgarian, short, the way the person said it, without the time: "да платя тока", "рожденият ден на мама", "да звънна на зъболекаря".
- date: the day of the first reminder, YYYY-MM-DD. Take "днес", "утре", "в петък" from the list of the days. A yearly date that has passed this year is next year.
- time: the hour, HH:MM on a 24-hour clock. "в 9 вечерта" = 21:00, "в 8 сутринта" = 08:00, "следобед в 3" = 15:00. No hour said: 09:00, and time_given false.
- time_given: true when the person said an hour.
- minutes_from_now: for "след 20 минути" 20, for "след 2 часа" 120; otherwise 0.
- repeat: "none"; "daily" ("всяка сутрин", "всеки ден"); "weekdays" ("в делничните дни"); "weekly" ("всеки понеделник": date is the next such day); "monthly" ("всеки месец на 5-ти"); "yearly" (a birthday, a name day, an anniversary).
- day_before: true for the yearly date of an event (a birthday, a name day, an anniversary): it is reminded the day before too.
When the earlier messages show a reminder that was proposed and the last message corrects it ("не, в 10", "по-добре в петък"), give the corrected reminder and keep what the correction does not change.`

// extractSchema is the JSON the model must answer with; the strings are bounded (a small model
// once repeated an unbounded field until max_tokens).
func extractSchema() json.RawMessage {
	repeats := []string{"none", Daily, Weekdays, Weekly, Monthly, Yearly}
	raw, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"is_reminder":      map[string]any{"type": "boolean"},
			"text":             map[string]any{"type": "string", "maxLength": MaxTextRunes},
			"date":             map[string]any{"type": "string", "maxLength": 10},
			"time":             map[string]any{"type": "string", "maxLength": 5},
			"time_given":       map[string]any{"type": "boolean"},
			"minutes_from_now": map[string]any{"type": "integer"},
			"repeat":           map[string]any{"type": "string", "enum": repeats},
			"day_before":       map[string]any{"type": "boolean"},
		},
		"required":             []string{"is_reminder", "text", "date", "time", "time_given", "minutes_from_now", "repeat", "day_before"},
		"additionalProperties": false,
	})
	return raw
}

// daysTable is the list of the days the prompt gives: "днес — вторник, 2026-11-24".
func daysTable(now time.Time, loc *time.Location) string {
	today := DayOf(now, loc)
	var b strings.Builder
	for i := 0; i < 8; i++ {
		d := today.AddDays(i)
		label := ""
		switch i {
		case 0:
			label = "днес — "
		case 1:
			label = "утре — "
		case 2:
			label = "вдругиден — "
		}
		fmt.Fprintf(&b, "%s%s, %s\n", label, WeekdayName(d.Weekday()), d)
	}
	return b.String()
}

// Extract asks the model for the fields of the request (the last message) after the earlier turns.
func (e *Extractor) Extract(ctx context.Context, message string, earlier []Turn, now time.Time, loc *time.Location) (Fields, error) {
	local := now.In(loc)
	clock := fmt.Sprintf("%s, %s, %s", WeekdayName(local.Weekday()), DayOf(now, loc), local.Format("15:04"))
	messages := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: fmt.Sprintf(extractPrompt, clock, daysTable(now, loc))}}
	for _, t := range earlier {
		role := openai.ChatMessageRoleUser
		if t.Role == openai.ChatMessageRoleAssistant {
			role = openai.ChatMessageRoleAssistant
		}
		messages = append(messages, openai.ChatCompletionMessage{Role: role, Content: t.Content})
	}
	messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: message})

	reqCtx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	resp, err := e.llm.ChatWithJSONSchema(reqCtx, openai.ChatCompletionRequest{
		Messages: messages,
		//lint:ignore SA1019 llama-server reads max_tokens; max_completion_tokens is OpenAI's own name.
		MaxTokens:   300,
		Temperature: 0.1,
	}, extractSchema())
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

// Draft is a reminder made from a request, ready to be said back and kept.
type Draft struct {
	Text  string
	Rule  Rule
	Next  time.Time
	Early bool
	// Corrected names what the code changed of the model's fields: "date", "time", "from_now",
	// "next_day" (an hour that has passed today, with no day said, is tomorrow's).
	Corrected []string
}

// Problems a request may have; each has its answer (ProblemText).
const (
	NotReminder = "not_reminder"
	NoText      = "no_text"
	Past        = "past"
	Invalid     = "invalid"
)

// ProblemText is the answer of the code to a request with a problem.
func ProblemText(problem string, d Draft, loc *time.Location) string {
	switch problem {
	case NoText:
		return "За какво да ти напомня? Напиши ми го с часа, например „напомни ми утре в 9 да платя тока“."
	case Past:
		return fmt.Sprintf("Това време вече мина (%s). За кога да го запиша?", onDay(d.Rule.First.Time(d.Rule.Minute, loc), loc))
	}
	return "Не разбрах кога да ти напомня. Напиши ми деня и часа, например „утре в 9“, „в петък в 18:30“, „след 20 минути“ или „всеки понеделник в 8“."
}

// Build makes the draft of the reminder from the model's fields and what the code reads in the
// message itself; problem is "" when the draft is good.
func Build(f Fields, message string, now time.Time, loc *time.Location) (d Draft, problem string) {
	if !f.IsReminder {
		return Draft{}, NotReminder
	}
	text := strings.Join(strings.Fields(f.Text), " ")
	text = strings.TrimRight(text, ".!")
	if utf8.RuneCountInString(text) > MaxTextRunes {
		text = string([]rune(text)[:MaxTextRunes]) + "…"
	}
	if text == "" {
		return Draft{}, NoText
	}
	d.Text = text
	repeat := f.Repeat
	if repeat == "none" {
		repeat = Once
	}
	d.Rule = Rule{Repeat: repeat, DayBefore: f.DayBefore && repeat == Yearly}
	reading := Read(message, now, loc)
	today := DayOf(now, loc)

	switch {
	case reading.FromNow > 0 || (f.MinutesFromNow > 0 && reading.Day == nil && reading.Minute == nil):
		from := reading.FromNow
		if from == 0 {
			from = time.Duration(f.MinutesFromNow) * time.Minute
		} else if f.MinutesFromNow == 0 || absDuration(time.Duration(f.MinutesFromNow)*time.Minute-from) > time.Minute {
			d.Corrected = append(d.Corrected, "from_now")
		}
		at := now.Add(from).In(loc).Truncate(time.Minute)
		if at.Before(now.Add(from)) {
			at = at.Add(time.Minute)
		}
		d.Rule.First = DayOf(at, loc)
		d.Rule.Minute = at.Hour()*60 + at.Minute()
	default:
		day, ok := ParseDay(strings.TrimSpace(f.Date))
		if reading.Day != nil && (!ok || day != *reading.Day) {
			if ok {
				d.Corrected = append(d.Corrected, "date")
			}
			day, ok = *reading.Day, true
		}
		if !ok {
			day = today
		}
		minute, mok := parseClock(f.Time)
		if reading.Minute != nil && (!mok || (minute != *reading.Minute && minute != *reading.Minute+12*60)) {
			if mok {
				d.Corrected = append(d.Corrected, "time")
			}
			minute, mok = *reading.Minute, true
		}
		if !mok || (!f.TimeGiven && reading.Minute == nil) {
			minute = 9 * 60
		}
		d.Rule.First, d.Rule.Minute = day, minute
	}

	if v := d.Rule.Valid(); v != "" {
		return d, Invalid
	}
	if d.Rule.First.Before(today) {
		switch d.Rule.Repeat {
		case Once:
			return d, Past
		case Yearly:
			for d.Rule.First.Before(today) {
				d.Rule.First.Year++
			}
			d.Rule.First.Day = min(d.Rule.First.Day, daysIn(d.Rule.First.Year, d.Rule.First.Month))
		default:
			d.Rule.First = today
		}
	}
	if d.Rule.Repeat == Once && !d.Rule.First.Time(d.Rule.Minute, loc).After(now) {
		if reading.DateSaid || reading.FromNow > 0 {
			return d, Past
		}
		// "В 9" said at nine in the evening, with no day: tomorrow's nine.
		d.Rule.First = d.Rule.First.AddDays(1)
		d.Corrected = append(d.Corrected, "next_day")
	}
	next, early, ok := d.Rule.Next(now, loc)
	if !ok {
		return d, Past
	}
	d.Next, d.Early = next, early
	return d, ""
}

// parseClock reads "09:00" or "9:00" into the minute of the day.
func parseClock(s string) (int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		if t, err = time.Parse("3:04", strings.TrimSpace(s)); err != nil {
			return 0, false
		}
	}
	return t.Hour()*60 + t.Minute(), true
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ProposalText is the question the code asks before it keeps the reminder.
func ProposalText(d Draft, now time.Time, loc *time.Location, telegram bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ще ти напомня: „%s“\n🗓 %s\n", d.Text, WhenText(d.Rule, d.Next, now, loc))
	where := "тук, в разговора"
	if telegram {
		where = "в Telegram и тук, в разговора"
	}
	fmt.Fprintf(&b, "\nТака ли? Кажи „да“ и ще го запиша (напомнянето идва %s). Ако нещо не е както трябва, кажи ми какво да поправя.", where)
	return b.String()
}
