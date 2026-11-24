package reminders

import (
	"fmt"
	"strings"
	"time"
)

// The words of the dates, in Bulgarian.
var (
	weekdayNames = [...]string{"неделя", "понеделник", "вторник", "сряда", "четвъртък", "петък", "събота"}
	monthNames   = [...]string{"", "януари", "февруари", "март", "април", "май", "юни", "юли", "август", "септември", "октомври", "ноември", "декември"}
)

// LateAfter: a reminder sent this much after its hour is said to be late (the computer was off).
const LateAfter = 10 * time.Minute

// WeekdayName is "сряда".
func WeekdayName(wd time.Weekday) string { return weekdayNames[wd] }

// onWeekday is "в сряда", "във вторник".
func onWeekday(wd time.Weekday) string {
	name := weekdayNames[wd]
	if strings.HasPrefix(name, "в") || strings.HasPrefix(name, "ф") {
		return "във " + name
	}
	return "в " + name
}

// everyWeekday is "всеки понеделник", "всяка сряда".
func everyWeekday(wd time.Weekday) string {
	switch wd {
	case time.Wednesday, time.Saturday, time.Sunday:
		return "всяка " + weekdayNames[wd]
	}
	return "всеки " + weekdayNames[wd]
}

// dayNumber is "5-ти", "1-ви", "22-ри", "7-ми": the day of the month as it is said.
func dayNumber(n int) string {
	switch {
	case n%10 == 1 && n != 11:
		return fmt.Sprintf("%d-ви", n)
	case n%10 == 2 && n != 12:
		return fmt.Sprintf("%d-ри", n)
	case (n%10 == 7 || n%10 == 8) && n != 17 && n != 18:
		return fmt.Sprintf("%d-ми", n)
	}
	return fmt.Sprintf("%d-ти", n)
}

// Clock is "09:00".
func Clock(minute int) string { return fmt.Sprintf("%02d:%02d", minute/60, minute%60) }

// DateText is "сряда, 25.11.2026".
func DateText(d Day) string {
	return fmt.Sprintf("%s, %02d.%02d.%d", weekdayNames[d.Weekday()], d.Day, int(d.Month), d.Year)
}

// relativeDay is "днес", "утре", "вдругиден" or "" for a day further off.
func relativeDay(d, today Day) string {
	switch d {
	case today:
		return "днес"
	case today.AddDays(1):
		return "утре"
	case today.AddDays(2):
		return "вдругиден"
	}
	return ""
}

// AtText is "утре, сряда, 25.11.2026, в 09:00": a sending, with the day said the way the person
// would say it when it is near.
func AtText(t time.Time, now time.Time, loc *time.Location) string {
	d := DayOf(t, loc)
	minute := t.In(loc).Hour()*60 + t.In(loc).Minute()
	when := DateText(d)
	if rel := relativeDay(d, DayOf(now, loc)); rel != "" {
		when = rel + ", " + when
	}
	return when + ", в " + Clock(minute)
}

// WhenText says when a reminder is sent: "утре, сряда, 25.11.2026, в 09:00", "всеки ден в 08:00
// (първия път — утре, сряда, 25.11.2026)". next is its first sending from now.
func WhenText(r Rule, next time.Time, now time.Time, loc *time.Location) string {
	first := AtText(next, now, loc)
	firstDay := first[:strings.LastIndex(first, ", в ")]
	at := "в " + Clock(r.Minute)
	switch r.Repeat {
	case Daily:
		return fmt.Sprintf("всеки ден %s (първия път — %s)", at, firstDay)
	case Weekdays:
		return fmt.Sprintf("всеки делничен ден (понеделник – петък) %s (първия път — %s)", at, firstDay)
	case Weekly:
		return fmt.Sprintf("%s %s (първия път — %s)", everyWeekday(r.First.Weekday()), at, firstDay)
	case Monthly:
		text := fmt.Sprintf("всеки месец на %s %s (първия път — %s)", dayNumber(r.First.Day), at, firstDay)
		if r.First.Day > 28 {
			text += "; в по-кратък месец — в последния му ден"
		}
		return text
	case Yearly:
		date := fmt.Sprintf("%d %s", r.First.Day, monthNames[r.First.Month])
		if r.DayBefore {
			return fmt.Sprintf("всяка година за %s: ден преди това и в самия ден, %s (първия път — %s)", date, at, firstDay)
		}
		return fmt.Sprintf("всяка година на %s %s (първия път — %s)", date, at, firstDay)
	}
	return first
}

// RepeatText is the repeat as the list of reminders names it: "всеки ден", "" for none.
func RepeatText(r Rule) string {
	switch r.Repeat {
	case Daily:
		return "всеки ден"
	case Weekdays:
		return "в делничните дни"
	case Weekly:
		return everyWeekday(r.First.Weekday())
	case Monthly:
		return "всеки месец на " + dayNumber(r.First.Day)
	case Yearly:
		return fmt.Sprintf("всяка година на %d %s", r.First.Day, monthNames[r.First.Month])
	}
	return ""
}

// SentText is the reminder as it arrives: "⏰ Напомняне: да платя тока". at is the time it was for
// and now the time it is sent; a sending that comes late says so.
func SentText(text string, r Rule, at time.Time, early bool, now time.Time, loc *time.Location) string {
	var head string
	switch {
	case early:
		head = fmt.Sprintf("⏰ Утре (%s)", DateText(Occurrence(at, true, loc)))
	case r.DayBefore:
		head = "⏰ Днес"
	default:
		head = "⏰ Напомняне"
	}
	if now.Sub(at) > LateAfter {
		head += fmt.Sprintf(" (закъсняло — трябваше да дойде %s; компютърът е бил изключен или бекендът спрян)", onDay(at, loc))
	}
	return head + ": " + text
}

// onDay is "в сряда, 25.11.2026, в 09:00".
func onDay(t time.Time, loc *time.Location) string {
	d := DayOf(t, loc)
	minute := t.In(loc).Hour()*60 + t.In(loc).Minute()
	return fmt.Sprintf("%s, %02d.%02d.%d, в %s", onWeekday(d.Weekday()), d.Day, int(d.Month), d.Year, Clock(minute))
}

// OnText is AtText said after a verb: "утре, сряда, 25.11.2026, в 09:00" or "в петък, 04.12.2026, в 09:00".
func OnText(t time.Time, now time.Time, loc *time.Location) string {
	if rel := relativeDay(DayOf(t, loc), DayOf(now, loc)); rel != "" {
		return AtText(t, now, loc)
	}
	return onDay(t, loc)
}

// OnDateText is "в петък, 01.01.2027", "във вторник, 05.01.2027".
func OnDateText(d Day) string {
	return fmt.Sprintf("%s, %02d.%02d.%d", onWeekday(d.Weekday()), d.Day, int(d.Month), d.Year)
}
