// Package reminders keeps the person's reminders and sends them in their hour (plan item И8,
// 2026-11-24).
//
// The model only turns the words of a request into the fields of a reminder (extract.go). The
// code checks them against what it can read in the words itself (check.go), says the date back
// with the day of the week and the year, and stores the reminder after the person's yes. The
// sender (runner.go) writes it into the conversation and sends it to Telegram in its hour; a
// reminder whose hour passed while the computer was off is sent at the next start, marked late.
package reminders

import (
	"fmt"
	"time"
	_ "time/tzdata" // the zone of the person on a machine without the zone files (Windows)
)

// Zone is the person's wall clock: Bremen (the clock of the owner's Windows machine).
const Zone = "Europe/Berlin"

// The repeats a reminder may have.
const (
	Once     = ""
	Daily    = "daily"
	Weekdays = "weekdays"
	Weekly   = "weekly"
	Monthly  = "monthly"
	Yearly   = "yearly"
)

// Repeats are the repeats in the order the model is offered them.
var Repeats = []string{Once, Daily, Weekdays, Weekly, Monthly, Yearly}

// lookAhead bounds the search of the next sending: a year and a bit holds an occurrence of every
// repeat.
const lookAhead = 400

// Location is the person's zone; it is always there (time/tzdata is built in).
func Location() *time.Location {
	loc, err := time.LoadLocation(Zone)
	if err != nil {
		panic(fmt.Sprintf("the zone %s is not built in: %v", Zone, err))
	}
	return loc
}

// Day is a date of the calendar, without a time and a zone.
type Day struct {
	Year  int
	Month time.Month
	Day   int
}

// DayOf is the date of t on the person's wall clock.
func DayOf(t time.Time, loc *time.Location) Day {
	y, m, d := t.In(loc).Date()
	return Day{y, m, d}
}

// ParseDay reads "2026-11-25".
func ParseDay(s string) (Day, bool) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Day{}, false
	}
	return Day{t.Year(), t.Month(), t.Day()}, true
}

// Time is the day at the minute of the day, on the wall clock of loc.
func (d Day) Time(minute int, loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, minute/60, minute%60, 0, 0, loc)
}

// AddDays is the day n days later (earlier when n < 0).
func (d Day) AddDays(n int) Day {
	t := time.Date(d.Year, d.Month, d.Day+n, 12, 0, 0, 0, time.UTC)
	return Day{t.Year(), t.Month(), t.Day()}
}

// Weekday is the day of the week of d.
func (d Day) Weekday() time.Weekday {
	return time.Date(d.Year, d.Month, d.Day, 12, 0, 0, 0, time.UTC).Weekday()
}

// Before tells whether d comes before e.
func (d Day) Before(e Day) bool {
	if d.Year != e.Year {
		return d.Year < e.Year
	}
	if d.Month != e.Month {
		return d.Month < e.Month
	}
	return d.Day < e.Day
}

// String is "2026-11-25".
func (d Day) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}

// UTC is the day as the time the database keeps for a DATE.
func (d Day) UTC() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// DayFromUTC is the day of a DATE read from the database.
func DayFromUTC(t time.Time) Day {
	y, m, dd := t.UTC().Date()
	return Day{y, m, dd}
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 12, 0, 0, 0, time.UTC).Day()
}

// Rule is when a reminder is sent: from First on, at Minute of the day, by Repeat; DayBefore sends
// it also on the day before each occurrence (a birthday: the day before and on the day).
type Rule struct {
	First     Day
	Minute    int
	Repeat    string
	DayBefore bool
}

// Valid tells what is wrong with the rule, "" when nothing is.
func (r Rule) Valid() string {
	if r.Minute < 0 || r.Minute >= 24*60 {
		return "the time of the day is not a time"
	}
	if r.First.Month < time.January || r.First.Month > time.December || r.First.Day < 1 || r.First.Day > daysIn(r.First.Year, r.First.Month) {
		return "the date is not a date"
	}
	switch r.Repeat {
	case Once, Daily, Weekdays, Weekly, Monthly, Yearly:
	default:
		return "the repeat is not one of the repeats"
	}
	return ""
}

// occursOn tells whether the rule has an occurrence on the day.
func (r Rule) occursOn(d Day) bool {
	if d.Before(r.First) {
		return false
	}
	switch r.Repeat {
	case Once:
		return d == r.First
	case Daily:
		return true
	case Weekdays:
		wd := d.Weekday()
		return wd != time.Saturday && wd != time.Sunday
	case Weekly:
		return d.Weekday() == r.First.Weekday()
	case Monthly:
		// The 31st is the last day of a shorter month.
		return d.Day == min(r.First.Day, daysIn(d.Year, d.Month))
	case Yearly:
		// The 29th of February is the 28th in a year that has none.
		return d.Month == r.First.Month && d.Day == min(r.First.Day, daysIn(d.Year, d.Month))
	}
	return false
}

// Next is the first sending of the rule after the time given (strictly), and whether it is the one
// of the day before; ok is false when there is none (a reminder that is over).
func (r Rule) Next(after time.Time, loc *time.Location) (at time.Time, early bool, ok bool) {
	start := DayOf(after, loc)
	if first := r.First.AddDays(-1); start.Before(first) {
		start = first
	}
	for i := 0; i <= lookAhead; i++ {
		d := start.AddDays(i)
		// The sending of the day before the occurrence of tomorrow comes first in the day d.
		if r.DayBefore && r.occursOn(d.AddDays(1)) {
			if t := d.Time(r.Minute, loc); t.After(after) {
				return t, true, true
			}
		}
		if r.occursOn(d) {
			if t := d.Time(r.Minute, loc); t.After(after) {
				return t, false, true
			}
		}
		if r.Repeat == Once && r.First.Before(d) {
			return time.Time{}, false, false
		}
	}
	return time.Time{}, false, false
}

// Occurrence is the day of the occurrence a sending at the time given is for: the day itself, or
// the day after for the sending of the day before.
func Occurrence(at time.Time, early bool, loc *time.Location) Day {
	d := DayOf(at, loc)
	if early {
		return d.AddDays(1)
	}
	return d
}
