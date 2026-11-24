package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/llm"
	"go-monolith/internal/reminders"
)

func TestWhichMessagesAskForAReminder(t *testing.T) {
	for _, yes := range []string{
		"напомни ми утре в 9 да платя тока",
		"Напомняй ми всяка сутрин в 8 да си пия хапчето",
		"можеш ли да ми напомниш в петък за срещата",
		"рожденият ден на мама е на 12 март",
		"remind me tomorrow at 9 to call the bank",
	} {
		assert.True(t, asksForReminder(yes), yes)
	}
	for _, no := range []string{
		"напомни ми какво говорихме вчера",
		"напомни ми за какво говорихме",
		"какви напомняния имам?",
		"махни напомнянето за тока",
		"рожденият ден на мама беше хубав",
		"как си днес?",
		"запиши в бележки.md: напомняне за тока",
	} {
		assert.False(t, asksForReminder(no), no)
	}
	assert.True(t, asksForReminderList("какви напомняния имам?"))
	assert.True(t, asksForReminderList("Покажи ми напомнянията"))
	assert.True(t, asksToRemoveReminder("махни напомнянето за тока"))
	assert.True(t, asksToRemoveReminder("изтрий напомнянето за срещата"))
}

type reminderEnv struct {
	*attachmentEnv
	o       *Orchestrator
	chat    *llm.MockLLM
	reader  *llm.MockLLM
	store   *reminders.Store
	wakes   int
	session string
}

// The clock of the tests: Tuesday, 24.11.2026, 20:41 in Berlin.
var reminderNow = time.Date(2026, 11, 24, 20, 41, 0, 0, reminders.Location())

func newReminderEnv(t *testing.T) *reminderEnv {
	t.Helper()
	e := newAttachmentEnv(t)
	chat := answeringModel()
	reader := &llm.MockLLM{}
	store := reminders.NewStore(e.db)
	env := &reminderEnv{attachmentEnv: e, chat: chat, reader: reader, store: store, session: "rem"}
	env.o = orchestratorWith(t, e, chat).WithHelpAnswer().WithReminders(store, reminders.NewExtractor(reader), func() { env.wakes++ })
	env.o.reminders.now = func() time.Time { return reminderNow }
	// A long conversation starts the rolling summary in the background.
	t.Cleanup(env.o.summaryWG.Wait)
	return env
}

// reads makes the model that reads a request answer with these fields.
func (e *reminderEnv) reads(t *testing.T, f reminders.Fields) {
	t.Helper()
	raw, err := json.Marshal(f)
	require.NoError(t, err)
	e.reader.Response = openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: string(raw)}}}}
}

func (e *reminderEnv) say(t *testing.T, message string) string {
	t.Helper()
	return (&taskRunEnv{attachmentEnv: e.attachmentEnv, o: e.o}).say(t, e.session, message)
}

func TestAReminderIsSaidBackWithTheYearAndKeptAfterTheYes(t *testing.T) {
	e := newReminderEnv(t)
	e.reads(t, reminders.Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "09:00", TimeGiven: true, Repeat: "none"})

	out := e.say(t, "напомни ми утре в 9 да платя тока")
	assert.Contains(t, out, "Ще ти напомня: „да платя тока“")
	assert.Contains(t, out, "утре, сряда, 25.11.2026, в 09:00")
	assert.Contains(t, out, "Така ли?")
	assert.Empty(t, e.chat.Requests, "the chat model was not asked")
	system := e.reader.LastRequest.Messages[0].Content
	assert.Contains(t, system, "вторник, 2026-11-24, 20:41", "the reader has the clock to the minute")
	assert.Contains(t, system, "утре — сряда, 2026-11-25", "and the days written out")

	active, err := e.store.Active(context.Background(), e.userID)
	require.NoError(t, err)
	assert.Empty(t, active, "nothing is kept before the yes")

	out = e.say(t, "да")
	assert.Contains(t, out, "Записах ✅")
	assert.Contains(t, out, "25.11.2026")
	assert.Equal(t, 1, e.wakes, "the sender is woken")
	active, err = e.store.Active(context.Background(), e.userID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "да платя тока", active[0].Text)
	assert.Equal(t, time.Date(2026, 11, 25, 9, 0, 0, 0, reminders.Location()), active[0].Next.In(reminders.Location()))

	out = e.say(t, "да")
	assert.NotContains(t, out, "Записах", "a second yes keeps nothing more")
}

func TestTheCodeFixesTheDayOfTheWeekTheModelGotWrong(t *testing.T) {
	e := newReminderEnv(t)
	// "в петък" read as Thursday.
	e.reads(t, reminders.Fields{IsReminder: true, Text: "да взема колата", Date: "2026-11-26", Time: "10:00", TimeGiven: true, Repeat: "none"})
	out := e.say(t, "напомни ми в петък в 10 да взема колата")
	assert.Contains(t, out, "петък, 27.11.2026, в 10:00")
}

func TestACorrectionReplacesTheProposal(t *testing.T) {
	e := newReminderEnv(t)
	e.reads(t, reminders.Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "09:00", TimeGiven: true, Repeat: "none"})
	e.say(t, "напомни ми утре в 9 да платя тока")

	e.reads(t, reminders.Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "10:00", TimeGiven: true, Repeat: "none"})
	out := e.say(t, "не, в 10")
	assert.Contains(t, out, "утре, сряда, 25.11.2026, в 10:00")
	msgs := e.reader.LastRequest.Messages
	require.GreaterOrEqual(t, len(msgs), 3)
	assert.Contains(t, msgs[len(msgs)-2].Content, "да платя тока", "the reader sees the proposal it corrects")

	e.say(t, "да")
	active, err := e.store.Active(context.Background(), e.userID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, 10, active[0].Next.In(reminders.Location()).Hour())
}

func TestAnotherMessageDropsTheReminderProposal(t *testing.T) {
	e := newReminderEnv(t)
	e.reads(t, reminders.Fields{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "09:00", TimeGiven: true, Repeat: "none"})
	e.say(t, "напомни ми утре в 9 да платя тока")

	out := e.say(t, "разкажи ми нещо интересно за звездите и галактиките, които са най-далеч от нас")
	assert.Contains(t, out, "Видях го.", "the chat model answers")
	p, err := e.store.Proposed(context.Background(), e.userID, e.session)
	require.NoError(t, err)
	assert.Nil(t, p)
	out = e.say(t, "да")
	assert.NotContains(t, out, "Записах")
}

func TestARecallIsNotAReminderAndTheModelAnswers(t *testing.T) {
	e := newReminderEnv(t)
	e.reads(t, reminders.Fields{IsReminder: false})
	out := e.say(t, "можеш ли да ми напомниш нещо от разговора ни за звездите")
	assert.Contains(t, out, "Видях го.")
}

func TestTheListAndTheRemoval(t *testing.T) {
	e := newReminderEnv(t)
	for _, f := range []reminders.Fields{
		{IsReminder: true, Text: "да платя тока", Date: "2026-11-25", Time: "09:00", TimeGiven: true, Repeat: "none"},
		{IsReminder: true, Text: "да проверя пощата", Date: "2026-11-30", Time: "08:00", TimeGiven: true, Repeat: reminders.Weekly},
		{IsReminder: true, Text: "рожденият ден на мама", Date: "2027-03-12", Time: "09:00", Repeat: reminders.Yearly, DayBefore: true},
	} {
		e.reads(t, f)
		e.say(t, "напомни ми "+f.Text)
		e.say(t, "да")
	}
	out := e.say(t, "какви напомняния имам?")
	assert.Contains(t, out, "Напомнянията ти (3)")
	assert.Contains(t, out, "1. „да платя тока“")
	assert.Contains(t, out, "2. „да проверя пощата“ — всеки понеделник")
	assert.Contains(t, out, "3. „рожденият ден на мама“ — всяка година на 12 март")

	out = e.say(t, "махни 2")
	assert.Contains(t, out, "Махнах напомнянето „да проверя пощата“")
	out = e.say(t, "махни напомнянето за тока")
	assert.Contains(t, out, "Махнах напомнянето „да платя тока“")
	active, err := e.store.Active(context.Background(), e.userID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "рожденият ден на мама", active[0].Text)
	out = e.say(t, "махни напомнянето за зъболекаря")
	assert.Contains(t, out, "Не намерих такова напомняне")
}

func TestTheButtonOfTheRemindersReadsAnyMessage(t *testing.T) {
	e := newReminderEnv(t)
	e.reads(t, reminders.Fields{IsReminder: true, Text: "да звънна на Иван", Date: "2026-11-27", Time: "18:00", TimeGiven: true, Repeat: "none"})
	code, out := (&taskRunEnv{attachmentEnv: e.attachmentEnv, o: e.o}).sayIn(t, e.session, "Иван в петък в 18", ModeReminder)
	require.Equal(t, 200, code, out)
	assert.Contains(t, out, "„да звънна на Иван“")
	assert.Contains(t, out, "петък, 27.11.2026, в 18:00")
	assert.Empty(t, e.chat.Requests)
}

func TestTheButtonOfTheRemindersIsOffWithoutThem(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	code, out := (&taskRunEnv{attachmentEnv: e, o: o}).sayIn(t, "rem", "утре в 9", ModeReminder)
	require.Equal(t, 200, code, out)
	assert.Contains(t, out, "Режимът „Напомняне“ сега не е включен")
}

func TestTheHelpListsTheRemindersWhenTheyAreOn(t *testing.T) {
	e := newReminderEnv(t)
	text := e.o.helpText()
	assert.Contains(t, text, "⏰ Напомняния")
	assert.Contains(t, text, "/remind")
}
