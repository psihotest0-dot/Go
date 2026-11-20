package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/llm"
	"go-monolith/internal/userfiles"
)

func TestRequestSinceIsTheLastRealRequestBeforeTheQuestion(t *testing.T) {
	u := func(c string) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: c}
	}
	a := func(c string) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: c}
	}
	t0 := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	at := func(minutes ...int) []time.Time {
		out := make([]time.Time, len(minutes))
		for i, m := range minutes {
			out[i] = t0.Add(time.Duration(m) * time.Minute)
		}
		return out
	}

	// the real chat of 2026-11-20: the file was made on the first request, the second one
	// ("запиши цялата статия") made nothing, and then the question
	history := []openai.ChatCompletionMessage{u("първо създай файла за да видя как изглежда"), a("Готово"), u("опитай да запишеш цялата статия"), a("Ще започна"), u("успя ли ?")}
	assert.Equal(t, t0.Add(1*time.Minute), requestSince(history, at(0, 0, 1, 1, 6)), "the request before the question, not the one that made the file")

	// "ok" in between is not a request: the check goes back past it
	withOk := []openai.ChatCompletionMessage{u("опитай да запишеш цялата статия"), a("Ще започна"), u("добре"), a("Започвам"), u("успя ли ?")}
	assert.Equal(t, t0, requestSince(withOk, at(0, 0, 2, 2, 6)))

	assert.True(t, requestSince(nil, nil).IsZero(), "no history: the zero time")
	short := []openai.ChatCompletionMessage{u("ок"), a("b"), u("успя ли ?")}
	assert.Equal(t, t0, requestSince(short, at(0, 1, 6)), "nothing long enough: the earliest message")
	assert.True(t, requestSince(history[4:], at(6)).IsZero(), "the question alone: nothing to compare with")
	assert.True(t, requestSince(history, at(0, 1)).IsZero(), "times that do not go with the messages")
	assert.Equal(t, t0, requestSince([]openai.ChatCompletionMessage{u("опитай да запишеш цялата статия"), a("b"), a("c"), u("q")}, at(0, 1, 2, 3)), "assistant messages are not counted")
}

func TestFilesChangedSince(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "files")
	store, err := userfiles.Open(dir, userfiles.Limits{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	reg := (&DefaultToolRegistry{}).WithFiles(store)
	user := uuid.New()
	_, err = store.Create(user, "a.md", "text")
	require.NoError(t, err)
	// the file is in the folder of the user, under the folder of the store: make it old
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, user.String(), "a.md"), old, old))

	changed, err := reg.FilesChangedSince(user, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.False(t, changed, "the file is two hours old")

	changed, err = reg.FilesChangedSince(user, time.Now().Add(-3*time.Hour))
	require.NoError(t, err)
	assert.True(t, changed, "and it is newer than three hours ago")

	changed, err = reg.FilesChangedSince(uuid.New(), time.Now().Add(-3*time.Hour))
	require.NoError(t, err)
	assert.False(t, changed, "another user has no files")

	changed, err = (&DefaultToolRegistry{}).FilesChangedSince(user, time.Time{})
	require.NoError(t, err)
	assert.False(t, changed, "without the folder there is nothing to have changed")
}

// statusRegistry is a tool registry that knows whether the files changed.
type statusRegistry struct {
	fakeToolRegistry
	changed bool
	err     error
	asked   int
}

func (s *statusRegistry) FilesChangedSince(uuid.UUID, time.Time) (bool, error) {
	s.asked++
	return s.changed, s.err
}

// statusCtx is the context of a turn of a user, in which the work was asked for an hour ago.
func statusCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := context.WithValue(t.Context(), api.ParsedUserIDKey, uuid.New())
	ctx, turn := withTurn(ctx, "s", false)
	turn.setRequestSince(time.Now().Add(-time.Hour))
	return ctx
}

func askStatus(t *testing.T, reg ToolRegistry, answer, question string) (string, *llm.MockLLM) {
	t.Helper()
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(answer)}}}
	o := newGroupOrchestrator(mock, reg)
	content, _, _, err := o.generateResponse(statusCtx(t),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: question}}, o.turnTools(), nil)
	require.NoError(t, err)
	return content, mock
}

// The real answers of 2026-11-20 (replays of the turn of 19:06): "Да, успях…" with no call
// and no change of a file.
func TestASuccessClaimToAStatusQuestionWithNoChangedFileIsCorrected(t *testing.T) {
	for _, answer := range []string{
		"Да, успях да извлека съдържанието и го структурирах по-подробно. Файлът `Lou_Polk_Skouras_2019.md` вече съдържа по-пълно описание на методологията.",
		"Да, успях да пренапиша файла, като вложих много по-подробно съдържание от първия източник.",
		"Осъщепих файла, като включих много по-детайлно съдържание. Можеш да провериш файла сега.",
	} {
		reg := &statusRegistry{changed: false}

		content, mock := askStatus(t, reg, answer, "успя ли ?")

		assert.Equal(t, answer+"\n\n"+statusCorrection, content, answer)
		assert.Len(t, mock.Requests, 1, "no retry: there is nothing to retry")
		assert.Equal(t, 1, reg.asked)
	}
}

func TestASuccessClaimIsLeftAloneWhenAFileChanged(t *testing.T) {
	answer := "Да, успях да пренапиша файла."
	content, _ := askStatus(t, &statusRegistry{changed: true}, answer, "успя ли ?")
	assert.Equal(t, answer, content)
}

func TestAnAnswerThatIsNotAClaimOfSuccessIsLeftAlone(t *testing.T) {
	for _, answer := range []string{
		"Не успях да запиша цялата статия в един файл — тя е твърде обемна.",
		"Не успех да запиша файла.",
		"Файлът още не е готов.",
		"Не мога да запиша цялата статия: пълният текст не е в разговора.",
		"Искаш ли да го запиша в нов файл?",
		"Кажи ми името на файла и го записвам.",
	} {
		reg := &statusRegistry{changed: false}
		content, _ := askStatus(t, reg, answer, "успя ли с файла?")
		assert.Equal(t, answer, content, answer)
	}
}

// "В процес на" is the promise, not the claim: its own correction.
func TestAPromiseToAStatusQuestionKeepsItsOwnCorrection(t *testing.T) {
	content, _ := askStatus(t, &statusRegistry{changed: false}, "В момента съм в процес на преобразуване на файла.", "успя ли ?")
	assert.Contains(t, content, stallCorrection)
	assert.NotContains(t, content, statusCorrection)
}

func TestAStatusQuestionAboutNoFileIsNotChecked(t *testing.T) {
	reg := &statusRegistry{changed: false}

	content, _ := askStatus(t, reg, "Да, намерих го — цената е 12 лева.", "успя ли да намериш цената?")

	assert.Equal(t, "Да, намерих го — цената е 12 лева.", content)
	assert.Equal(t, 0, reg.asked, "no file is mentioned: the folder is not asked about")
}

func TestTheStatusCheckNeedsToKnowSinceWhenAndWhoseFiles(t *testing.T) {
	answer := "Да, успях да пренапиша файла."
	mock := func() *llm.MockLLM {
		return &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{{textChunk(answer)}}}
	}
	msg := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "успя ли ?"}}

	// no time recorded: the turn does not know what to compare with
	ctx, _ := withTurn(context.WithValue(t.Context(), api.ParsedUserIDKey, uuid.New()), "s", false)
	o := newGroupOrchestrator(mock(), &statusRegistry{changed: false})
	content, _, _, err := o.generateResponse(ctx, msg, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Equal(t, answer, content, "no time")

	// no user in the context
	ctx, turn := withTurn(t.Context(), "s", false)
	turn.setRequestSince(time.Now().Add(-time.Hour))
	o = newGroupOrchestrator(mock(), &statusRegistry{changed: false})
	content, _, _, err = o.generateResponse(ctx, msg, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Equal(t, answer, content, "no user")

	// a registry that cannot tell
	o = newGroupOrchestrator(mock(), &fakeToolRegistry{})
	content, _, _, err = o.generateResponse(statusCtx(t), msg, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Equal(t, answer, content, "no checker")

	// the folder cannot be read: no accusation
	reg := &statusRegistry{changed: false, err: assert.AnError}
	o = newGroupOrchestrator(mock(), reg)
	content, _, _, err = o.generateResponse(statusCtx(t), msg, o.turnTools(), nil)
	require.NoError(t, err)
	assert.Equal(t, answer, content, "a failure to look is not a finding")
}

// A tool was called in the turn: the answer may be about what it did.
func TestAStatusAnswerAfterAToolCallIsLeftAlone(t *testing.T) {
	reg := &statusRegistry{changed: false}
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "call_1", "create_memory", `{"content":"пише статия"}`)},
		{textChunk("Да, успях да запиша бележката за файла.")},
	}}
	o := newGroupOrchestrator(mock, reg)

	content, _, _, err := o.generateResponse(statusCtx(t),
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "успя ли ?"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Да, успях да запиша бележката за файла.", content)
}

// Found in 16 replays of the turn of 19:06 (2026-11-20): a refusal at the start does not
// excuse a claim after it.
func TestARefusalFollowedByAClaimIsStillCheckedAgainstTheFolder(t *testing.T) {
	for _, answer := range []string{
		"Не е възможно да запиша буквално всяка дума в един файл. Вместо това аз преструктурирах файла с пълния логически поток на изследването.",
		"Нямам достъп до целия текст, но започнах файла с каквото знам.",
	} {
		content, _ := askStatus(t, &statusRegistry{changed: false}, answer, "успя ли ?")
		assert.Equal(t, answer+"\n\n"+statusCorrection, content, answer)
	}

	// the same refusals without a claim are left alone
	for _, answer := range []string{
		"Не е възможно да запиша буквално всяка дума в един файл.",
		"Нямам достъп до целия текст на статията, затова файлът е същият.",
	} {
		content, _ := askStatus(t, &statusRegistry{changed: false}, answer, "успя ли ?")
		assert.Equal(t, answer, content, answer)
	}
}

func TestDoneVerb(t *testing.T) {
	for _, s := range []string{"успях", "Преструктурирах файла", "аз обнових го", "направих го.", "започнах да чета"} {
		assert.True(t, doneVerb.MatchString(s), s)
	}
	for _, s := range []string{"не мога", "ще го направя", "файлът е готов", "то е", "ах!", "мях"} {
		assert.False(t, doneVerb.MatchString(s), s)
	}
}
