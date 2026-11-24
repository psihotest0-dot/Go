package reminders

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var berlin = Location()

// at is a time on the person's clock.
func at(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, berlin)
}

func TestNextOfEachRepeat(t *testing.T) {
	// Tuesday, 24.11.2026, 20:41.
	now := at(2026, 11, 24, 20, 41)
	cases := []struct {
		name  string
		rule  Rule
		want  time.Time
		early bool
	}{
		{"once tomorrow", Rule{First: Day{2026, 11, 25}, Minute: 9 * 60}, at(2026, 11, 25, 9, 0), false},
		{"daily from today, hour passed", Rule{First: Day{2026, 11, 24}, Minute: 8 * 60, Repeat: Daily}, at(2026, 11, 25, 8, 0), false},
		{"daily from today, hour ahead", Rule{First: Day{2026, 11, 24}, Minute: 21 * 60, Repeat: Daily}, at(2026, 11, 24, 21, 0), false},
		{"weekdays skip the weekend", Rule{First: Day{2026, 11, 27}, Minute: 7 * 60, Repeat: Weekdays}, at(2026, 11, 27, 7, 0), false},
		{"weekly on Monday", Rule{First: Day{2026, 11, 30}, Minute: 8 * 60, Repeat: Weekly}, at(2026, 11, 30, 8, 0), false},
		{"monthly on the 31st is the 30th of November", Rule{First: Day{2026, 10, 31}, Minute: 10 * 60, Repeat: Monthly}, at(2026, 11, 30, 10, 0), false},
		{"yearly birthday: the day before first", Rule{First: Day{2027, 3, 12}, Minute: 9 * 60, Repeat: Yearly, DayBefore: true}, at(2027, 3, 11, 9, 0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, early, ok := c.rule.Next(now, berlin)
			require.True(t, ok)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.early, early)
		})
	}
}

func TestABirthdayIsSentTheDayBeforeAndOnTheDayEveryYear(t *testing.T) {
	r := Rule{First: Day{2027, 3, 12}, Minute: 9 * 60, Repeat: Yearly, DayBefore: true}
	t1, e1, _ := r.Next(at(2027, 3, 11, 9, 0).Add(-time.Second), berlin)
	t2, e2, _ := r.Next(t1, berlin)
	t3, e3, _ := r.Next(t2, berlin)
	assert.Equal(t, at(2027, 3, 11, 9, 0), t1)
	assert.True(t, e1)
	assert.Equal(t, at(2027, 3, 12, 9, 0), t2)
	assert.False(t, e2)
	assert.Equal(t, at(2028, 3, 11, 9, 0), t3)
	assert.True(t, e3)
	assert.Equal(t, Day{2028, 3, 12}, Occurrence(t3, e3, berlin))
}

func TestAOnceReminderIsOverAfterItsTime(t *testing.T) {
	r := Rule{First: Day{2026, 11, 25}, Minute: 9 * 60}
	_, _, ok := r.Next(at(2026, 11, 25, 9, 0), berlin)
	assert.False(t, ok)
}

func TestTheTwentyNinthOfFebruaryIsTheTwentyEighthInAYearWithoutIt(t *testing.T) {
	r := Rule{First: Day{2028, 2, 29}, Minute: 9 * 60, Repeat: Yearly}
	got, _, ok := r.Next(at(2028, 3, 1, 0, 0), berlin)
	require.True(t, ok)
	assert.Equal(t, at(2029, 2, 28, 9, 0), got)
}

func TestADailyReminderKeepsItsHourAcrossSummerTime(t *testing.T) {
	// Summer time begins on 28.03.2027 in Berlin.
	r := Rule{First: Day{2027, 3, 27}, Minute: 8 * 60, Repeat: Daily}
	got, _, _ := r.Next(at(2027, 3, 27, 9, 0), berlin)
	assert.Equal(t, 8, got.In(berlin).Hour())
	assert.Equal(t, 28, got.In(berlin).Day())
}

func TestTheWordsOfTheDates(t *testing.T) {
	now := at(2026, 11, 24, 20, 41)
	assert.Equal(t, "утре, сряда, 25.11.2026, в 09:00", AtText(at(2026, 11, 25, 9, 0), now, berlin))
	assert.Equal(t, "петък, 04.12.2026, в 18:30", AtText(at(2026, 12, 4, 18, 30), now, berlin))
	assert.Equal(t, "всеки понеделник в 08:00 (първия път — понеделник, 30.11.2026)",
		WhenText(Rule{First: Day{2026, 11, 30}, Minute: 8 * 60, Repeat: Weekly}, at(2026, 11, 30, 8, 0), now, berlin))
	assert.Equal(t, "всяка сряда в 08:00 (първия път — утре, сряда, 25.11.2026)",
		WhenText(Rule{First: Day{2026, 11, 25}, Minute: 8 * 60, Repeat: Weekly}, at(2026, 11, 25, 8, 0), now, berlin))
	assert.Contains(t, WhenText(Rule{First: Day{2027, 3, 12}, Minute: 9 * 60, Repeat: Yearly, DayBefore: true}, at(2027, 3, 11, 9, 0), now, berlin),
		"всяка година за 12 март: ден преди това и в самия ден, в 09:00 (първия път — четвъртък, 11.03.2027)")
	assert.Equal(t, "5-ти", dayNumber(5))
	assert.Equal(t, "21-ви", dayNumber(21))
	assert.Equal(t, "12-ти", dayNumber(12))
	assert.Equal(t, "22-ри", dayNumber(22))
	assert.Equal(t, "7-ми", dayNumber(7))
	assert.Equal(t, "във вторник", onWeekday(time.Tuesday))
}

func TestTheSentTextSaysWhenItIsLate(t *testing.T) {
	due := at(2026, 11, 25, 9, 0)
	r := Rule{First: Day{2026, 11, 25}, Minute: 9 * 60}
	assert.Equal(t, "⏰ Напомняне: да платя тока", SentText("да платя тока", r, due, false, due.Add(30*time.Second), berlin))
	late := SentText("да платя тока", r, due, false, due.Add(3*time.Hour), berlin)
	assert.Contains(t, late, "закъсняло — трябваше да дойде в сряда, 25.11.2026, в 09:00")
	b := Rule{First: Day{2027, 3, 12}, Minute: 9 * 60, Repeat: Yearly, DayBefore: true}
	assert.Equal(t, "⏰ Утре (петък, 12.03.2027): рожденият ден на мама",
		SentText("рожденият ден на мама", b, at(2027, 3, 11, 9, 0), true, at(2027, 3, 11, 9, 0), berlin))
	assert.Equal(t, "⏰ Днес: рожденият ден на мама",
		SentText("рожденият ден на мама", b, at(2027, 3, 12, 9, 0), false, at(2027, 3, 12, 9, 0), berlin))
}
