package reminders

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWhatTheCodeReadsInTheWords(t *testing.T) {
	// Tuesday, 24.11.2026, 20:41.
	now := at(2026, 11, 24, 20, 41)
	day := func(y int, m time.Month, d int) *Day { return &Day{y, m, d} }
	minute := func(h, m int) *int { v := h*60 + m; return &v }
	cases := []struct {
		msg     string
		day     *Day
		minute  *int
		fromNow time.Duration
	}{
		{"напомни ми утре в 9 да платя тока", day(2026, 11, 25), minute(9, 0), 0},
		{"напомни ми в петък в 18:30 за срещата", day(2026, 11, 27), minute(18, 30), 0},
		{"напомни ми във вторник да звънна", day(2026, 12, 1), nil, 0},
		{"напомни ми вдругиден", day(2026, 11, 26), nil, 0},
		{"напомни ми довечера в 10 да изнеса боклука", day(2026, 11, 24), minute(22, 0), 0},
		{"напомни ми в 8 сутринта", nil, minute(8, 0), 0},
		{"напомни ми след 20 минути да спра фурната", nil, nil, 20 * time.Minute},
		{"напомни ми след 2 часа", nil, nil, 2 * time.Hour},
		{"напомни ми след час", nil, nil, time.Hour},
		{"напомни ми след половин час", nil, nil, 30 * time.Minute},
		{"на 12.03 е рожденият ден на мама", day(2027, 3, 12), nil, 0},
		{"рожденият ден на мама е на 12 март", day(2027, 3, 12), nil, 0},
		{"на 5.12.2026 в 14:00 имам час при зъболекаря", day(2026, 12, 5), minute(14, 0), 0},
		// "в 9.05" is an hour, not the 9th of May.
		{"напомни ми утре в 9.05", day(2026, 11, 25), minute(9, 5), 0},
		{"в сряда в 6 и половина сутринта ми напомни за прегледа", day(2026, 11, 25), minute(6, 30), 0},
		{"напомни ми в 7 и четвърт вечерта", nil, minute(19, 15), 0},
		// "2 часа" after "след" is no hour of the day, "5 март" no hour either.
		{"всеки понеделник в 8 да проверя пощата", day(2026, 11, 30), minute(8, 0), 0},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			r := Read(c.msg, now, berlin)
			assert.Equal(t, c.day, r.Day, "day")
			assert.Equal(t, c.minute, r.Minute, "minute")
			assert.Equal(t, c.fromNow, r.FromNow, "from now")
		})
	}
}

func TestTheCodeCorrectsTheDayTheModelGotWrong(t *testing.T) {
	now := at(2026, 11, 24, 20, 41)
	// The model took "в петък" for Thursday.
	f := Fields{IsReminder: true, Text: "да взема колата от сервиза", Date: "2026-11-26", Time: "10:00", TimeGiven: true, Repeat: "none"}
	d, problem := Build(f, "напомни ми в петък в 10 да взема колата от сервиза", now, berlin)
	require.Empty(t, problem)
	assert.Equal(t, at(2026, 11, 27, 10, 0), d.Next)
	assert.Equal(t, []string{"date"}, d.Corrected)
}

func TestTheCodeAddsTheMinutesItself(t *testing.T) {
	now := at(2026, 11, 24, 20, 41).Add(17 * time.Second)
	f := Fields{IsReminder: true, Text: "да спра фурната", Date: "2026-11-24", Time: "21:00", MinutesFromNow: 20, Repeat: "none"}
	d, problem := Build(f, "напомни ми след 20 минути да спра фурната", now, berlin)
	require.Empty(t, problem)
	assert.Equal(t, at(2026, 11, 24, 21, 2), d.Next)
}

func TestAnEveningHourTheModelTookFromTheWordsIsKept(t *testing.T) {
	now := at(2026, 11, 24, 10, 0)
	// "в 8" with "да гледам мача" at ten in the morning: the model said 20:00, the code read 8.
	f := Fields{IsReminder: true, Text: "да гледам мача", Date: "2026-11-24", Time: "20:00", TimeGiven: true, Repeat: "none"}
	d, problem := Build(f, "напомни ми в 8 да гледам мача", now, berlin)
	require.Empty(t, problem)
	assert.Equal(t, at(2026, 11, 24, 20, 0), d.Next)
	assert.Empty(t, d.Corrected)
}

func TestAnHourThatPassedTodayWithNoDaySaidIsTomorrows(t *testing.T) {
	now := at(2026, 11, 24, 20, 41)
	f := Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-24", Time: "09:00", TimeGiven: true, Repeat: "none"}
	d, problem := Build(f, "напомни ми в 9 сутринта да платя тока", now, berlin)
	require.Empty(t, problem)
	assert.Equal(t, at(2026, 11, 25, 9, 0), d.Next)
	assert.Contains(t, d.Corrected, "next_day")
}

func TestADatePassedIsAProblem(t *testing.T) {
	now := at(2026, 11, 24, 20, 41)
	f := Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-24", Time: "09:00", TimeGiven: true, Repeat: "none"}
	_, problem := Build(f, "напомни ми днес в 9 да платя тока", now, berlin)
	assert.Equal(t, Past, problem)
}

func TestABirthdayIsYearlyWithTheDayBefore(t *testing.T) {
	now := at(2026, 11, 24, 20, 41)
	// The model gave this year's date, which has passed.
	f := Fields{IsReminder: true, Text: "рожденият ден на мама", Date: "2026-03-12", Time: "09:00", Repeat: Yearly, DayBefore: true}
	d, problem := Build(f, "рожденият ден на мама е на 12 март, напомняй ми", now, berlin)
	require.Empty(t, problem)
	assert.Equal(t, Rule{First: Day{2027, 3, 12}, Minute: 9 * 60, Repeat: Yearly, DayBefore: true}, d.Rule)
	assert.Equal(t, at(2027, 3, 11, 9, 0), d.Next)
	assert.True(t, d.Early)
}

func TestNotAReminderAndNoText(t *testing.T) {
	now := at(2026, 11, 24, 20, 41)
	_, problem := Build(Fields{IsReminder: false}, "напомни ми какво говорихме вчера", now, berlin)
	assert.Equal(t, NotReminder, problem)
	_, problem = Build(Fields{IsReminder: true, Text: "  "}, "напомни ми утре", now, berlin)
	assert.Equal(t, NoText, problem)
}

func TestTheProposalSaysTheYear(t *testing.T) {
	now := at(2026, 11, 24, 20, 41)
	f := Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "09:00", TimeGiven: true, Repeat: "none"}
	d, _ := Build(f, "напомни ми утре в 9 да платя тока", now, berlin)
	text := ProposalText(d, now, berlin, true)
	assert.Contains(t, text, "Ще ти напомня: „да платя тока“")
	assert.Contains(t, text, "утре, сряда, 25.11.2026, в 09:00")
	assert.Contains(t, text, "в Telegram и тук")
}
