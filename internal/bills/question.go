package bills

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go-monolith/internal/reminders"
)

// The questions about the bills that the code answers (phase 2, "като счетоводител"): a sum over a
// period ("колко платих за ток тази година?"), the list of a period, the unpaid ones, and two
// periods side by side ("сравни ноември с октомври"). The numbers are the code's; the model at
// most adds one sentence about them (advisor.go).

// What a question asks for.
const (
	AskTotal   = "total"
	AskList    = "list"
	AskUnpaid  = "unpaid"
	AskCompare = "compare"
	// AskWhere: where the bills are kept ("имат ли копие в базата или само в bills.txt?"). The model,
	// asked on 2026-11-25, said they were only in a text file of its own - it does not know that the
	// code keeps them in the database.
	AskWhere = "where"
)

// Period is a span of days, From and To included, and how the answer names it.
type Period struct {
	From, To reminders.Day
	Label    string
}

// Question is a question about the bills as the code read it.
type Question struct {
	Ask    string
	Kind   string // "" for every kind
	Period Period
	// Other is the second period of a comparison.
	Other *Period
}

// questionMaxRunes: a question about the bills is a sentence.
const questionMaxRunes = 160

var (
	// billContext: the message is about the person's bills or what they pay.
	billContext = regexp.MustCompile(`(?i)(?:сметк|фактур|платих|плащам|платил|плащал|харч|разход|дълж|неплатен|изтегл|струваха|струва\s+ми|bills?\b|invoices?\b|spent|expenses)`)
	totalWords  = regexp.MustCompile(`(?i)(?:колко|сбор|общо|сума|сумата|how\s+much|total)`)
	listWords   = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:кои|какви|покажи|изреди|списък|list|which)(?:[^\p{L}]|$)`)
	unpaidWords = regexp.MustCompile(`(?i)(?:неплатен|не\s+съм\s+(?:ги\s+|я\s+)?платил|не\s+са\s+платени|дълж|чакат\s+плащане|unpaid|not\s+paid)`)
	compareWord = regexp.MustCompile(`(?i)(?:сравн|спрямо|в\s+сравнение|повече\s+ли|по-малко\s+ли|compare|versus|\bvs\b)`)
	// whereKept: where they are kept - "къде записа сметките", "имат ли копие в базата данни",
	// "пазиш ли ги", "в базата ли са". whereAsk: and it is a question.
	whereKept = regexp.MustCompile(`(?i)(?:баз[аи]|база\s+данни|копие|пазиш|пазят|пази\s+ли|съхран|записа(?:ла)?(?:[^\p{L}]|$)|database|stored|kept)`)
	whereAsk  = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:къде|ли|where|do\s+you|are\s+they)(?:[^\p{L}]|$)|\?`)

	thisMonth = regexp.MustCompile(`(?i)(?:този|тоя|текущия|this)\s+(?:месец|month)`)
	lastMonth = regexp.MustCompile(`(?i)(?:миналия|предишния|last|previous)\s+(?:месец|month)`)
	thisYear  = regexp.MustCompile(`(?i)(?:тази|тая|текущата|this)\s+(?:година|year)|(?:от\s+началото\s+на\s+годината)`)
	lastYear  = regexp.MustCompile(`(?i)(?:миналата|предишната|last|previous)\s+(?:година|year)|лани`)
	yearWord  = regexp.MustCompile(`(?:^|[^\d])(20\d{2})(?:[^\d]|$)`)
	monthWord = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(януари|февруари|март|април|май|юни|юли|август|септември|октомври|ноември|декември)(?:[^\p{L}]|$)`)
)

var monthsBG = map[string]time.Month{"януари": 1, "февруари": 2, "март": 3, "април": 4, "май": 5, "юни": 6, "юли": 7,
	"август": 8, "септември": 9, "октомври": 10, "ноември": 11, "декември": 12}

var monthNamesBG = [...]string{"", "януари", "февруари", "март", "април", "май", "юни", "юли", "август", "септември", "октомври", "ноември", "декември"}

// kindWords name a kind in the person's words.
var kindWords = []struct {
	re   *regexp.Regexp
	kind string
}{
	{regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:ток|тока|електричеств\p{L}*|strom)(?:[^\p{L}]|$)`), "electricity"},
	{regexp.MustCompile(`(?i)(?:телефон\p{L}*|интернет\p{L}*|мобилн\p{L}*|gsm)`), "phone_internet"},
	{regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:газ|газта|газа)(?:[^\p{L}]|$)`), "gas"},
	{regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:вода|водата)(?:[^\p{L}]|$)`), "water"},
	{regexp.MustCompile(`(?i)(?:отопление\p{L}*|парно\p{L}*)`), "heating"},
	{regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:наем\p{L}*|miete)`), "rent"},
	{regexp.MustCompile(`(?i)(?:застрах\p{L}*|versicherung)`), "insurance"},
	{regexp.MustCompile(`(?i)(?:зъболекар\p{L}*|зъбо\p{L}*|zahnarzt)`), "dentist"},
	{regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:лекар\p{L}*|доктор\p{L}*|болниц\p{L}*|arzt)`), "doctor"},
	{regexp.MustCompile(`(?i)(?:аптек\p{L}*|лекарств\p{L}*|apotheke)`), "pharmacy"},
	{regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:кола|колата|автомобил\p{L}*|бензин\p{L}*|гориво)(?:[^\p{L}]|$)`), "car"},
	{regexp.MustCompile(`(?i)(?:данъ\p{L}*|такс\p{L}*|steuer)`), "tax"},
	{regexp.MustCompile(`(?i)(?:абонамент\p{L}*)`), "subscription"},
	{regexp.MustCompile(`(?i)(?:покупк\p{L}*|пазар\p{L}*)`), "shopping"},
}

func monthPeriod(year int, m time.Month) Period {
	from := reminders.Day{Year: year, Month: m, Day: 1}
	to := reminders.Day{Year: year, Month: m, Day: daysInMonth(year, m)}
	return Period{From: from, To: to, Label: monthNamesBG[m] + " " + strconv.Itoa(year)}
}

func yearPeriod(year int) Period {
	return Period{From: reminders.Day{Year: year, Month: 1, Day: 1}, To: reminders.Day{Year: year, Month: 12, Day: 31}, Label: strconv.Itoa(year) + " г."}
}

func daysInMonth(year int, m time.Month) int {
	return time.Date(year, m+1, 0, 12, 0, 0, 0, time.UTC).Day()
}

// ReadQuestion is the question about the bills in message, ok false when it is none (the model
// answers it). today is the person's day.
func ReadQuestion(message string, today reminders.Day) (Question, bool) {
	m := strings.TrimSpace(message)
	if m == "" || utf8.RuneCountInString(m) > questionMaxRunes || !billContext.MatchString(m) {
		return Question{}, false
	}
	q := Question{}
	for _, k := range kindWords {
		if k.re.MatchString(m) {
			q.Kind = k.kind
			break
		}
	}
	switch {
	case whereKept.MatchString(m) && whereAsk.MatchString(m):
		q.Ask = AskWhere
		return q, true
	case unpaidWords.MatchString(m):
		q.Ask = AskUnpaid
	case compareWord.MatchString(m):
		q.Ask = AskCompare
	case totalWords.MatchString(m):
		q.Ask = AskTotal
	case listWords.MatchString(m):
		q.Ask = AskList
	default:
		return Question{}, false
	}

	// The periods the message names, in the order it names them.
	var periods []Period
	months := monthWord.FindAllStringSubmatchIndex(m, -1)
	year := today.Year
	if y := yearWord.FindStringSubmatch(m); y != nil {
		year, _ = strconv.Atoi(y[1])
	}
	for _, idx := range months {
		name := strings.ToLower(m[idx[2]:idx[3]])
		mon := monthsBG[name]
		y := year
		if yearWord.FindStringSubmatch(m) == nil && mon > today.Month {
			// "през декември" said in November is the last one.
			y = today.Year - 1
		}
		periods = append(periods, monthPeriod(y, mon))
	}
	if len(periods) == 0 {
		switch {
		case thisMonth.MatchString(m):
			periods = append(periods, monthPeriod(today.Year, today.Month))
		case lastMonth.MatchString(m):
			prev := reminders.Day{Year: today.Year, Month: today.Month, Day: 1}.AddDays(-1)
			periods = append(periods, monthPeriod(prev.Year, prev.Month))
		case lastYear.MatchString(m):
			periods = append(periods, yearPeriod(today.Year-1))
		case thisYear.MatchString(m):
			periods = append(periods, yearPeriod(today.Year))
		case yearWord.MatchString(m):
			periods = append(periods, yearPeriod(year))
		}
	}

	switch q.Ask {
	case AskCompare:
		switch len(periods) {
		case 0:
			// "сравни с миналия месец": this month and the one before.
			cur := monthPeriod(today.Year, today.Month)
			prev := reminders.Day{Year: today.Year, Month: today.Month, Day: 1}.AddDays(-1)
			p := monthPeriod(prev.Year, prev.Month)
			q.Period, q.Other = cur, &p
		case 1:
			if len(months) == 0 && lastMonth.MatchString(m) {
				// "сравни с миналия месец": this month and the last one.
				cur := monthPeriod(today.Year, today.Month)
				q.Period, q.Other = cur, &periods[0]
				break
			}
			if len(months) == 0 && lastYear.MatchString(m) {
				cur := yearPeriod(today.Year)
				q.Period, q.Other = cur, &periods[0]
				break
			}
			// "сравни ноември" - with the month (or the year) before it.
			p := periods[0]
			q.Period = p
			var o Period
			if p.From.Month == 1 && p.To.Month == 12 && p.From.Day == 1 && p.To.Day == 31 {
				o = yearPeriod(p.From.Year - 1)
			} else {
				prev := p.From.AddDays(-1)
				o = monthPeriod(prev.Year, prev.Month)
			}
			q.Other = &o
		default:
			q.Period, q.Other = periods[0], &periods[1]
		}
	case AskUnpaid:
		// The unpaid ones of any time, unless a period is named.
		if len(periods) > 0 {
			q.Period = periods[0]
		}
	default:
		if len(periods) > 0 {
			q.Period = periods[0]
		} else {
			// "колко са сметките?" - this year.
			q.Period = yearPeriod(today.Year)
		}
	}
	return q, true
}
