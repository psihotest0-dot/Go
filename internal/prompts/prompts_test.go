package prompts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryEmbeddedTemplateLoads(t *testing.T) {
	names := TemplateNames()
	require.NotEmpty(t, names)
	for _, want := range []string{"chat_system.tmpl", "memory_reflector.tmpl", "oracle_system.tmpl", "rolling_summary.tmpl", "rolling_summary_input.tmpl"} {
		assert.Contains(t, names, want)
	}

	// An empty override directory means the embedded copies only, whatever the
	// working directory holds.
	t.Chdir(t.TempDir())
	manager := NewTemplateManager("")
	require.NoError(t, manager.Preload(names...))
}

// The prompt is the same whatever the line endings of the checkout were: on
// Windows git turns the templates into CRLF files and they are embedded as they
// are, which put 111 carriage returns (about 120 tokens) into the chat prompt.
func TestRenderedPromptsHaveNoCarriageReturns(t *testing.T) {
	manager := NewTemplateManager("")
	data := map[string]interface{}{
		"CurrentDate": "2026-11-02 10:00:00", "Rules": []string{"правило"}, "TopMemories": []string{"факт"},
		"RollingSummary": "обобщение", "RelevantMemories": []string{"спомен"}, "Chunks": []string{"откъс"},
		"PreviousSummary": "старо", "MessagesText": "нови",
	}
	for _, name := range TemplateNames() {
		out, err := manager.Render(name, data)
		require.NoError(t, err, name)
		assert.NotContains(t, out, "\r", name)
	}
}

func TestOverrideWithCRLFRendersWithoutCarriageReturns(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rolling_summary.tmpl"), []byte("първи ред\r\nвтори ред\r\n"), 0o644))

	out, err := NewTemplateManager(dir).Render("rolling_summary.tmpl", nil)

	require.NoError(t, err)
	assert.Equal(t, "първи ред\nвтори ред\n", out)
}

func TestEmbeddedTemplatesRender(t *testing.T) {
	manager := NewTemplateManager("")

	chat, err := manager.Render("chat_system.tmpl", map[string]interface{}{
		"CurrentDate":      "2026-11-01 10:00:00",
		"Rules":            []string{"Говори на български"},
		"TopMemories":      []string{"Живее в София"},
		"RollingSummary":   "обобщение-X",
		"RelevantMemories": []string{"спомен-Y"},
		"Chunks":           []string{"откъс-Z"},
	})
	require.NoError(t, err)
	for _, want := range []string{"2026-11-01 10:00:00", "Говори на български", "Живее в София", "обобщение-X", "спомен-Y", "откъс-Z"} {
		assert.Contains(t, chat, want)
	}

	instructions, err := manager.Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, instructions)

	input, err := manager.Render("rolling_summary_input.tmpl", map[string]interface{}{
		"PreviousSummary": "старо-резюме",
		"MessagesText":    "нови-съобщения",
	})
	require.NoError(t, err)
	assert.Contains(t, input, "старо-резюме")
	assert.Contains(t, input, "нови-съобщения")

	withoutPrevious, err := manager.Render("rolling_summary_input.tmpl", map[string]interface{}{
		"PreviousSummary": "",
		"MessagesText":    "нови-съобщения",
	})
	require.NoError(t, err)
	assert.NotContains(t, withoutPrevious, "Предишно обобщение")

	_, err = manager.Render("memory_reflector.tmpl", nil)
	require.NoError(t, err)
}

// chatPrompt is the chat prompt of a bare message: no memories, summaries or rules.
func chatPrompt(t *testing.T) string {
	t.Helper()
	out, err := NewTemplateManager("").Render("chat_system.tmpl", map[string]interface{}{"CurrentDate": "2026-11-07 19:00:00"})
	require.NoError(t, err)
	return out
}

// The tone of the chat (task 59, measured on 2026-11-07 on 13 messages): a close
// register, a personal reaction to the feeling first, no analysis from the side.
// The rules that keep it from going too far stay.
func TestChatPromptKeepsTheCloseRegisterAndTheRulesThatLimitIt(t *testing.T) {
	chat := chatPrompt(t)

	for name, want := range map[string]string{
		"speaks to the person as to a close one": "Говориш на „ти“",
		"reacts to the feeling first":            "първото ти изречение е жива човешка реакция",
		"no general remark in the third person":  "не говори за него в трето лице",
		"plain correct Bulgarian":                "правилен български с прости, добре познати думи",
		"a different reaction every time":        "реагирай различно всеки път",
		"examples show the tone, not the words":  "не повтаряй техни изрази",
		"compliments must be earned":             "Compliments must be earned",
		"feminine forms for herself":             "always use feminine grammatical forms",
		"ends with a statement, not a question":  "End the reply with a statement",
		"does not reuse earlier openings":        "Do not reuse words, phrases, or sentence openings",
	} {
		assert.Contains(t, chat, want, name)
	}
}

// Facts of the world that the library does not answer go to the web, not to the
// memory of the model (2026-11-11, Gemma 4 12B, tool calls and answers measured on
// 28 questions): without the rule the model named "Кутев, 2994 м" as the highest peak
// of Rila in 2 of 2 tries, with it "Мусала, 2925 м" in 2 of 2 after a web search, and
// a final of the world cup was once given to the wrong opponent. The exceptions keep
// arithmetic, plain words and conversation (and the books, which the library answers)
// away from the web: 0 of 18 questions about books and 0 of 9 trivial or chat
// messages went to it.
func TestChatPromptSendsFactsOfTheWorldToTheWeb(t *testing.T) {
	chat := chatPrompt(t)

	for name, want := range map[string]string{
		"facts the library did not answer are not given from memory": "do not answer them from memory",
		"the web is searched": "Call web_search",
		"the best page is read when the snippets do not answer": "call web_fetch on the best page",
		"the source is named":                               "name the source in a few words",
		"arithmetic, words and conversation need no search": "No search is needed for plain arithmetic",
	} {
		assert.Contains(t, chat, want, name)
	}
}

// Gemma follows an example to the letter. With a question allowed at the end 14 of 14
// answers ended with one, and an example for a situation the person is likely to
// write about came back word for word 4 times in 4. So no example reply ends with a
// question, and no example is a message people send as it stands.
func TestChatPromptExamplesDoNotTeachQuestionsOrInviteCopying(t *testing.T) {
	chat := chatPrompt(t)

	replies := regexp.MustCompile(`Лили: „([^\n]*)“\n`).FindAllStringSubmatch(chat, -1)
	require.GreaterOrEqual(t, len(replies), 4, "the examples are there")
	for _, reply := range replies {
		assert.False(t, strings.HasSuffix(strings.TrimSpace(reply[1]), "?"), "an example ends with a question: %q", reply[1])
	}

	for _, sentence := range []string{
		"Уморен съм днес", "Днес всичко ми идва в повече", "Понякога вечер ми става самотно", "Най-накрая завърших проекта",
	} {
		assert.NotContains(t, chat, "Потребител: „"+sentence, "copied word for word in the measurement")
	}
}

// The background reflector saved "сега съм на работа" and "още се излежавам и пия кафе"
// as facts about the owner, and they reached every prompt for days. On the owner's 45
// messages and 7 lasting facts (2026-11-14, Gemma 4 12B) this rule took out every passing
// state and kept the 7 facts (plan item P43).
func TestMemoryReflectorLeavesPassingStatesOut(t *testing.T) {
	reflector, err := NewTemplateManager("").Render("memory_reflector.tmpl", nil)
	require.NoError(t, err)
	for name, want := range map[string]string{
		"passing states are not saved":  "НЕ записвай моментни състояния",
		"with the examples that got in": "сега съм на работа",
		"and what a lasting fact is":    "ще е вярно и след месец",
	} {
		assert.Contains(t, reflector, want, name)
	}
}

func TestUnknownTemplateIsAnError(t *testing.T) {
	_, err := NewTemplateManager("").Render("no_such_template.tmpl", nil)
	require.Error(t, err)
}

func TestOverrideDirectoryWinsAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rolling_summary.tmpl")
	require.NoError(t, os.WriteFile(path, []byte("override-v1"), 0o644))

	manager := NewTemplateManager(dir)

	got, err := manager.Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, "override-v1", got)

	// A template missing from the override directory comes from the embedded set.
	embedded, err := NewTemplateManager("").Render("memory_reflector.tmpl", nil)
	require.NoError(t, err)
	got, err = manager.Render("memory_reflector.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, embedded, got)

	// An edited file is picked up without a restart.
	require.NoError(t, os.WriteFile(path, []byte("override-v2"), 0o644))
	later := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, later, later))
	got, err = manager.Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.Equal(t, "override-v2", got)
}

func TestBrokenOverrideIsReportedAtPreload(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chat_system.tmpl"), []byte("{{ .Unclosed "), 0o644))

	err := NewTemplateManager(dir).Preload("chat_system.tmpl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chat_system.tmpl")
}

func TestEmptyOverrideDoesNotReadTheWorkingDirectory(t *testing.T) {
	// A stray file with a template's name in the working directory must not
	// replace the embedded template when no override directory is configured.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rolling_summary.tmpl"), []byte("stray-file"), 0o644))
	t.Chdir(dir)

	got, err := NewTemplateManager("").Render("rolling_summary.tmpl", nil)
	require.NoError(t, err)
	assert.NotEqual(t, "stray-file", got)
}
