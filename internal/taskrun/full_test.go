package taskrun

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
)

func TestTheTextOfADocumentIsPutTogetherWithoutTheOverlaps(t *testing.T) {
	chunks := []db.DocumentChunk{
		{ChunkType: "raw_text", Text: "Първото изречение е тук. Второто изречение продължава нататък"},
		{ChunkType: "concept_summary", Text: "резюме на понятие, не е текстът"},
		{ChunkType: "raw_text", Text: "Второто изречение продължава нататък и свършва. Третото."},
		{ChunkType: "raw_text", Text: " Без застъпване."},
	}
	assert.Equal(t, "Първото изречение е тук. Второто изречение продължава нататък и свършва. Третото. Без застъпване.", documentText(chunks))
	assert.Equal(t, 0, overlap("abc", "abd"), "a short coincidence is no overlap")
}

func TestPartsEndAtAParagraphOrASentence(t *testing.T) {
	para := strings.Repeat("Едно изречение с думи. ", 30) // ~690 runes
	text := strings.Repeat(para+"\n\n", 10)
	parts := splitParts(text, 1000)
	require.Greater(t, len(parts), 4)
	for i, p := range parts {
		assert.LessOrEqual(t, utf8.RuneCountInString(p), 1200, i)
		assert.True(t, strings.HasSuffix(p, "."), "part %d ends with a sentence: %q", i, p[max(0, len(p)-20):])
	}
	assert.Equal(t, strings.Join(strings.Fields(text), " "), strings.Join(strings.Fields(strings.Join(parts, " ")), " "), "nothing is lost")
	assert.Equal(t, []string{"кратко"}, splitParts("  кратко ", 1000))
	assert.Empty(t, splitParts("   ", 1000))
}

const partNotesAnswer = "- Частта казва, че доходността е 2.5%.\n- Изследването обхваща 1990 година.\n- Методът е регресия."

// three parts of the document: their numbers are 2.5 and 1990 each.
func threeParts() []string {
	part := strings.Repeat("Доходността е 2.5% през 1990 година, по метода на регресията. ", 160) // ~10 000 runes
	return []string{part, part, part}
}

func TestAWholeDocumentIsReadPartByPartIntoOneFile(t *testing.T) {
	e := newEnv(t, partNotesAnswer)
	doc := e.document(t, "long-paper.pdf", "completed", threeParts()...)
	e.start(t, KindFull, "full", doc)

	require.True(t, e.runner.pass(e.ctx))
	p := e.plan(t, "full")
	require.Len(t, p.Steps, 1)
	assert.Equal(t, taskplan.StepDoing, p.Steps[0].Status, "one part a pass: the plan shows it in work")
	assert.Equal(t, 1, p.Steps[0].PartsDone)
	assert.GreaterOrEqual(t, p.Steps[0].Parts, 3)
	name := p.Steps[0].File
	assert.Equal(t, "long-paper - подробни бележки.md", name)
	assert.Contains(t, e.read(t, name), "## Част 1 от", "the file grows after each part")

	e.walk(t)
	p = e.plan(t, "full")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status)
	assert.Equal(t, p.Steps[0].Parts, p.Steps[0].PartsDone)
	text := e.read(t, name)
	for i := 1; i <= p.Steps[0].Parts; i++ {
		assert.Contains(t, text, "## Част ", i)
	}
	assert.Equal(t, p.Steps[0].Parts, strings.Count(text, "## Част "))
	assert.Contains(t, text, "прочетен целият")
	assert.Contains(t, p.Steps[0].Evidence, "прочетен целият")
	assert.Len(t, e.llm.requests, p.Steps[0].Parts, "one request a part")
	for _, req := range e.llm.requests {
		assert.Contains(t, req.Messages[1].Content, "Част ", "the model gets the part")
		assert.Less(t, utf8.RuneCountInString(req.Messages[1].Content), PartRunes+PartRunes/5+200)
	}
	reports := e.assistantMessages(t, "full")
	require.Len(t, reports, 1)
	assert.Contains(t, reports[0], name)
}

func TestAPartWithNothingToNoteGetsNoSection(t *testing.T) {
	e := newEnv(t, noneWord, partNotesAnswer)
	doc := e.document(t, "paper.pdf", "completed", threeParts()...)
	e.start(t, KindFull, "full", doc)
	e.walk(t)
	p := e.plan(t, "full")
	text := e.read(t, p.Steps[0].File)
	assert.Equal(t, p.Steps[0].Parts-1, strings.Count(text, "## Част "), "the first part (a table of contents) has none")
	assert.NotContains(t, text, "## Част 1 от")
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status)
	assert.Empty(t, p.Steps[0].Note, "a part with nothing to note is no failure")
	assert.NotContains(t, p.Steps[0].Evidence, "не станаха")
}

func TestANumberFromElsewhereInAPartIsAskedAgainThenLeftOut(t *testing.T) {
	bad := partNotesAnswer + "\n- Растежът е 7.77%."
	e := newEnv(t, bad, bad)
	doc := e.document(t, "paper.pdf", "completed", threeParts()[0])
	e.start(t, KindFull, "full", doc)
	e.walk(t)
	p := e.plan(t, "full")
	text := e.read(t, p.Steps[0].File)
	assert.NotContains(t, text, "7.77")
	assert.Contains(t, text, "2.5%")
	require.Len(t, e.llm.requests, 2, "asked again once")
	assert.Contains(t, e.llm.requests[1].Messages[3].Content, "7.77")
}

func TestAStopComesBetweenTwoParts(t *testing.T) {
	e := newEnv(t, partNotesAnswer)
	doc := e.document(t, "paper.pdf", "completed", threeParts()...)
	e.start(t, KindFull, "full", doc)
	require.True(t, e.runner.pass(e.ctx))
	stopped, err := e.plans.StopRun(e.ctx, e.user, "full")
	require.NoError(t, err)
	require.True(t, stopped)
	e.walk(t)
	assert.Len(t, e.llm.requests, 1, "no part after the stop")
}

func TestARestartGoesOnWithTheNextPart(t *testing.T) {
	e := newEnv(t, partNotesAnswer)
	doc := e.document(t, "paper.pdf", "completed", threeParts()...)
	e.start(t, KindFull, "full", doc)
	require.True(t, e.runner.pass(e.ctx))
	require.True(t, e.runner.pass(e.ctx))

	again := New(e.db, e.plans, nil, e.llm, e.files) // the backend started again
	for i := 0; i < 20 && again.pass(e.ctx); i++ {
	}
	p := e.plan(t, "full")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	text := e.read(t, p.Steps[0].File)
	assert.Equal(t, 1, strings.Count(text, "## Част 1 от"), "no part twice")
	assert.Equal(t, p.Steps[0].Parts, strings.Count(text, "## Част "))
}

// The code review of 2026-11-27: the file was made before the first part was read, so a restart
// while the model read it left an empty file, and the part read again after it made a second one.
func TestARestartWhileTheFirstPartIsReadLeavesOneFile(t *testing.T) {
	e := newEnv(t, partNotesAnswer)
	doc := e.document(t, "paper.pdf", "completed", threeParts()...)
	e.start(t, KindFull, "full", doc)
	ctx, stop := context.WithCancel(e.ctx)
	e.llm.onCall, e.llm.err = stop, context.Canceled // the backend stops while the model reads
	e.runner.pass(ctx)
	e.llm.onCall, e.llm.err = nil, nil
	list, err := e.files.List(e.user)
	require.NoError(t, err)
	assert.Empty(t, list, "no file before the first notes")

	again := New(e.db, e.plans, nil, e.llm, e.files) // the backend started again
	for i := 0; i < 20 && again.pass(e.ctx); i++ {
	}
	p := e.plan(t, "full")
	assert.Equal(t, taskplan.PlanDone, p.Status)
	list, err = e.files.List(e.user)
	require.NoError(t, err)
	require.Len(t, list, 1, "one file")
	assert.Equal(t, "paper - подробни бележки.md", p.Steps[0].File)
	assert.Equal(t, 1, strings.Count(e.read(t, p.Steps[0].File), "## Част 1 от"))
}

// Every part with nothing to note: the step still names its file.
func TestADocumentWithNothingToNoteStillHasItsFile(t *testing.T) {
	e := newEnv(t, noneWord)
	doc := e.document(t, "paper.pdf", "completed", threeParts()...)
	e.start(t, KindFull, "full", doc)
	e.walk(t)
	p := e.plan(t, "full")
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status)
	require.NotEmpty(t, p.Steps[0].File)
	assert.Contains(t, e.read(t, p.Steps[0].File), "прочетен целият")
	assert.Contains(t, p.Steps[0].Evidence, p.Steps[0].File)
}

func TestADocumentTooLongToReadWholeFailsWithItsReason(t *testing.T) {
	e := newEnv(t, partNotesAnswer)
	texts := make([]string, 0, MaxParts+8)
	for range MaxParts + 8 {
		texts = append(texts, threeParts()[0])
	}
	doc := e.document(t, "book.pdf", "completed", texts...)
	e.start(t, KindFull, "full", doc)
	e.walk(t)
	p := e.plan(t, "full")
	assert.Equal(t, taskplan.StepFailed, p.Steps[0].Status)
	assert.Contains(t, p.Steps[0].Note, "твърде дълъг")
	assert.Empty(t, e.llm.requests)
}

func TestWhenTheFileIsFullTheNotesGoOnInANewOne(t *testing.T) {
	e := newEnv(t, partNotesAnswer)
	small, err := userfiles.Open(t.TempDir(), userfiles.Limits{MaxFileBytes: 900})
	require.NoError(t, err)
	t.Cleanup(func() { _ = small.Close() })
	e.runner = New(e.db, e.plans, nil, e.llm, small)
	doc := e.document(t, "paper.pdf", "completed", threeParts()...)
	e.start(t, KindFull, "full", doc)
	for i := 0; i < 20 && e.runner.pass(e.ctx); i++ {
	}
	p := e.plan(t, "full")
	assert.Equal(t, taskplan.StepDone, p.Steps[0].Status)
	assert.Contains(t, p.Steps[0].File, "продължение")
	assert.Contains(t, p.Steps[0].Evidence, "paper - подробни бележки.md", "both files are named")
}
