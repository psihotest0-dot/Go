package reminders

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What the code reads in the words itself. The model's date is checked against it: a small model
// that is told that today is Tuesday still gets "в петък" wrong now and then, and it cannot add
// "след 20 минути" to a clock it does not see. Where the code reads a day or an hour, its reading
// wins; everything else is the model's, and the person sees all of it before the reminder is kept.

const word = `(?:^|[^\p{L}])`
const wordEnd = `(?:[^\p{L}]|$)`

var (
	todayWords    = regexp.MustCompile(`(?i)` + word + `(?:днес|довечера|тази\s+вечер|tonight|today)` + wordEnd)
	tomorrowWords = regexp.MustCompile(`(?i)` + word + `(?:утре|tomorrow)` + wordEnd)
	dayAfterWords = regexp.MustCompile(`(?i)` + word + `(?:вдругиден|в\s*други\s+ден)` + wordEnd)

	// weekdayWords: "в петък", "във вторник", "следващия понеделник", "всеки четвъртък".
	weekdayWords = regexp.MustCompile(`(?i)` + word + `(понеделник|вторник|сряда|четвъртък|петък|събота|неделя)` + wordEnd)

	// inWords: "след 20 минути", "след 2 часа", "след час", "след половин час", "след 3 дни".
	inWords = regexp.MustCompile(`(?i)` + word + `след\s+(?:(\d{1,4})\s*|(половин)\s+)?(минут\p{L}*|мин\.?|час\p{L}*|ден|дни|дена|седмиц\p{L}*)` + wordEnd)

	// dotDate: "25.11", "25.11.2026", "5.3.27".
	dotDate = regexp.MustCompile(`(?:^|[^\d.])(\d{1,2})\.(\d{1,2})(?:\.(\d{2}|\d{4}))?(?:[^\d.]|\.(?:\s|$)|$)`)
	// monthDate: "12 март", "на 3 януари 2027".
	monthDate = regexp.MustCompile(`(?i)(?:^|[^\d])(\d{1,2})\s+(януари|февруари|март|април|май|юни|юли|август|септември|октомври|ноември|декември)(?:\s+(\d{4}))?` + wordEnd)

	// clockWords: "в 9", "в 21:30", "в 8 часа", "в 7.30", "към 6". The hour must follow "в", "във",
	// "към", "около" or "at", so that "5 март" and "2 часа" are no hours.
	clockWords = regexp.MustCompile(`(?i)` + word + `(?:в|във|към|около|at)\s+(\d{1,2})(?:[:.](\d{2}))?(?:\s*(?:ч\.?|часа))?(?:\s+и\s+(половина|четвърт))?` + wordEnd)
	// eveningWords and morningWords say which half of the day an hour of 1 to 12 is in.
	eveningWords = regexp.MustCompile(`(?i)` + word + `(?:вечерта|довечера|вечер|следобед|след\s+обед|следобяд|pm)` + wordEnd)
	morningWords = regexp.MustCompile(`(?i)` + word + `(?:сутринта|сутрин|заранта|am)` + wordEnd)
)

var weekdayOfName = map[string]time.Weekday{
	"понеделник": time.Monday, "вторник": time.Tuesday, "сряда": time.Wednesday, "четвъртък": time.Thursday,
	"петък": time.Friday, "събота": time.Saturday, "неделя": time.Sunday,
}

var monthOfName = map[string]time.Month{
	"януари": time.January, "февруари": time.February, "март": time.March, "април": time.April, "май": time.May, "юни": time.June,
	"юли": time.July, "август": time.August, "септември": time.September, "октомври": time.October, "ноември": time.November, "декември": time.December,
}

// Reading is what the code read in the words: a day, an hour, or a time from now. Zero fields are
// what it did not read.
type Reading struct {
	Day     *Day
	Minute  *int
	FromNow time.Duration
	// DateSaid: the words name a date or a day ("утре", "в петък", "25.11"), not only an hour.
	DateSaid bool
}

// Read is what the code reads in message, at now on the wall clock of loc.
func Read(message string, now time.Time, loc *time.Location) Reading {
	var r Reading
	today := DayOf(now, loc)
	lower := strings.ToLower(message)

	if m := inWords.FindStringSubmatch(lower); m != nil {
		n := 1
		if m[1] != "" {
			n, _ = strconv.Atoi(m[1])
		}
		unit := m[3]
		var d time.Duration
		switch {
		case strings.HasPrefix(unit, "мин"):
			d = time.Duration(n) * time.Minute
		case strings.HasPrefix(unit, "час"):
			d = time.Duration(n) * time.Hour
			if m[2] != "" {
				d = 30 * time.Minute
			}
		case strings.HasPrefix(unit, "седмиц"):
			d = time.Duration(n) * 7 * 24 * time.Hour
		default:
			d = time.Duration(n) * 24 * time.Hour
		}
		if d > 0 && d <= 366*24*time.Hour {
			r.FromNow = d
			r.DateSaid = true
			return r
		}
	}

	switch {
	case dayAfterWords.MatchString(lower):
		d := today.AddDays(2)
		r.Day = &d
	case tomorrowWords.MatchString(lower):
		d := today.AddDays(1)
		r.Day = &d
	case todayWords.MatchString(lower):
		d := today
		r.Day = &d
	}
	if r.Day == nil {
		if d, ok := readDate(lower, today); ok {
			r.Day = &d
		}
	}
	if r.Day == nil {
		if m := weekdayWords.FindStringSubmatch(lower); m != nil {
			wd := weekdayOfName[m[1]]
			// The nearest such day after today: "в петък" said on a Friday is a week later.
			ahead := (int(wd) - int(today.Weekday()) + 7) % 7
			if ahead == 0 {
				ahead = 7
			}
			d := today.AddDays(ahead)
			r.Day = &d
		}
	}
	r.DateSaid = r.Day != nil

	if m := clockWords.FindStringSubmatch(lower); m != nil {
		h, _ := strconv.Atoi(m[1])
		mm := 0
		if m[2] != "" {
			mm, _ = strconv.Atoi(m[2])
		}
		switch {
		case m[2] == "" && m[3] == "половина":
			mm = 30
		case m[2] == "" && m[3] == "четвърт":
			mm = 15
		}
		if h <= 23 && mm <= 59 {
			if h >= 1 && h <= 11 && eveningWords.MatchString(lower) && !morningWords.MatchString(lower) {
				h += 12
			}
			minute := h*60 + mm
			r.Minute = &minute
		}
	}
	return r
}

// readDate is a date written out: "25.11", "25.11.2026", "12 март", "12 март 2027". A date without
// a year that has passed this year is the one of next year.
func readDate(lower string, today Day) (Day, bool) {
	var day, month, year int
	if m := monthDate.FindStringSubmatch(lower); m != nil {
		day, _ = strconv.Atoi(m[1])
		month = int(monthOfName[m[2]])
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
		}
	} else if m := dotDateNotClock(lower); m != nil {
		day, _ = strconv.Atoi(m[1])
		month, _ = strconv.Atoi(m[2])
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
			if year < 100 {
				year += 2000
			}
		}
	} else {
		return Day{}, false
	}
	if month < 1 || month > 12 || day < 1 {
		return Day{}, false
	}
	explicitYear := year != 0
	if !explicitYear {
		year = today.Year
	}
	if day > daysIn(year, time.Month(month)) && !(month == 2 && day == 29) {
		return Day{}, false
	}
	d := Day{year, time.Month(month), min(day, daysIn(year, time.Month(month)))}
	if !explicitYear && d.Before(today) {
		d = Day{year + 1, time.Month(month), min(day, daysIn(year+1, time.Month(month)))}
	}
	return d, true
}

// clockBefore: what comes before an hour ("в 9.05" is five past nine, "на 9.05" the 9th of May).
var clockBefore = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:в|във|към|около|at)\s*$`)

// dotDateNotClock is the first "25.11" of the text that is not an hour after "в".
func dotDateNotClock(lower string) []string {
	for _, idx := range dotDate.FindAllStringSubmatchIndex(lower, -1) {
		if clockBefore.MatchString(lower[:idx[2]]) {
			continue
		}
		m := make([]string, len(idx)/2)
		for i := range m {
			if idx[2*i] >= 0 {
				m[i] = lower[idx[2*i]:idx[2*i+1]]
			}
		}
		return m
	}
	return nil
}
