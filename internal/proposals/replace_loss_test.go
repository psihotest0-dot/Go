package proposals

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeptPercent(t *testing.T) {
	for name, tc := range map[string]struct {
		old, next string
		want      int
	}{
		"everything is kept and more is added": {"а\nб\nв", "а\nб\nв\nг\nд", 100},
		"nothing is kept":                      {"а\nб\nв", "x\ny", 0},
		"a third is kept":                      {"а\nб\nв", "а\nнещо друго", 33},
		"half is kept":                         {"а\nб\nв\nг", "б\nг\nнов ред", 50},
		"the order does not matter":            {"а\nб\nв", "в\nб\nа", 100},
		"white space at the ends is ignored":   {"  а  \nб\r", "а\nб", 100},
		"empty lines are not lines":            {"а\n\n\nб\n", "а\nб", 100},
		"a text with no line keeps all of it":  {"\n \n", "нещо", 100},
		"empty old text":                       {"", "нещо", 100},
		"a line repeated in the new text":      {"а\nб", "а\nа\nа", 50},
		"the same text":                        {"а\nб", "а\nб", 100},
		"a line cut in two is not kept":        {"един дълъг ред", "един дълъг\nред", 0},
	} {
		assert.Equal(t, tc.want, keptPercent(tc.old, tc.next), name)
	}
}

// A replacement shows what it costs: how long the text is now, how long the new one is, and how
// much of the old one it keeps - the chat of 2026-11-21 had a "keep adding" answered by a
// replacement that kept a third of the file, and the user confirmed it without knowing.
func TestAReplacementSaysWhatItCosts(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	old := "първи ред на файла\nвтори ред на файла\nтрети ред на файла\nчетвърти ред на файла"
	_, err := store.Create(e.userID, "a.txt", old)
	require.NoError(t, err)

	p, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "a.txt", "първи ред на файла\nнещо съвсем друго", nil)
	require.NoError(t, err)

	assert.Equal(t, len([]rune(old)), p.OldRunes)
	assert.Equal(t, 25, p.KeptPercent)
	assert.Contains(t, p.Summary, "Замяна на файла „a.txt“", "the start of the line is as it was")
	assert.Contains(t, p.Summary, "ВНИМАНИЕ: губи се по-голямата част от сегашния текст")
	assert.Contains(t, p.Summary, "запазва само 25 %")
	assert.Contains(t, p.Summary, "новият е 36", "the length of the new text")
	assert.NotContains(t, p.Summary, "\n")

	// What the user sees later, from the database, is the same line.
	list, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, p.Summary, list[0].Summary)
	assert.Equal(t, 25, list[0].KeptPercent)
	assert.Equal(t, p.OldRunes, list[0].OldRunes)
}

func TestAReplacementThatKeepsTheTextIsNotAWarning(t *testing.T) {
	e, store, _ := newFilesEnv(t)
	_, err := store.Create(e.userID, "a.txt", "един\nдва\nтри\nчетири")
	require.NoError(t, err)

	// The old text and a new part: what "keep adding" should have been, in one write.
	more, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "a.txt", "един\nдва\nтри\nчетири\nпет", nil)
	require.NoError(t, err)
	assert.Equal(t, 100, more.KeptPercent)
	assert.Contains(t, more.Summary, "запазва 100 % от редовете му")
	assert.NotContains(t, more.Summary, "ВНИМАНИЕ")

	// Exactly half is still not a loss; one line less than half is.
	half, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "a.txt", "един\nдва\nнов", nil)
	require.NoError(t, err)
	assert.Equal(t, LossyKeptPercent, half.KeptPercent)
	assert.NotContains(t, half.Summary, "ВНИМАНИЕ")
	lossy, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "a.txt", "един\nнов", nil)
	require.NoError(t, err)
	assert.Equal(t, 25, lossy.KeptPercent)
	assert.Contains(t, lossy.Summary, "ВНИМАНИЕ")
}

func TestOnlyAReplacementOfAFileWithTextHasFigures(t *testing.T) {
	e, store, _ := newFilesEnv(t)

	created, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteCreate, "new.txt", "текст", nil)
	require.NoError(t, err)
	assert.Zero(t, created.OldRunes)
	assert.Equal(t, "Нов файл „new.txt“ (5 знака): текст", created.Summary)

	_, err = store.Create(e.userID, "a.txt", "стар текст")
	require.NoError(t, err)
	appended, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteAppend, "a.txt", "още", nil)
	require.NoError(t, err)
	assert.Zero(t, appended.OldRunes)
	assert.Equal(t, "Добавяне към файла „a.txt“ (3 знака): още", appended.Summary)

	// A file with nothing in it has nothing to lose.
	_, err = store.Create(e.userID, "empty.txt", " ")
	if err == nil {
		replaced, err := e.svc.ProposeWrite(e.ctx, e.userID, "s", WriteReplace, "empty.txt", "текст", nil)
		require.NoError(t, err)
		assert.Zero(t, replaced.OldRunes)
		assert.NotContains(t, replaced.Summary, "ВНИМАНИЕ")
	}
}

// A proposal made before the figures existed has none in its payload and is shown as it was.
func TestAReplacementWithoutFiguresIsSummarisedAsBefore(t *testing.T) {
	got := writeSummary(WritePayload{Mode: WriteReplace, Name: "a.txt", Content: "new text"})

	assert.Equal(t, "Замяна на файла „a.txt“ (8 знака): new text", got)
	assert.False(t, strings.Contains(got, "ВНИМАНИЕ"))
}
