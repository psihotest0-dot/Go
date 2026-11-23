package taskrun

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/db"
	"go-monolith/internal/taskplan"
	"go-monolith/internal/userfiles"
)

func chunk(i int, text string) db.DocumentChunk {
	return db.DocumentChunk{ID: uuid.New(), ChunkIndex: int32(i), Text: text}
}

func TestASmallDocumentIsGivenWhole(t *testing.T) {
	chunks := []db.DocumentChunk{chunk(0, "a"), chunk(1, "b"), chunk(2, "c")}
	assert.Equal(t, chunks, pickPassages(chunks, nil))
}

func TestALargeDocumentGivesItsStartTheNearPassagesAndItsEndInItsOrder(t *testing.T) {
	part := strings.Repeat("x", 1000)
	var chunks []db.DocumentChunk
	for i := 0; i < 60; i++ {
		chunks = append(chunks, chunk(i, part))
	}
	near := []string{chunks[30].ID.String(), chunks[2].ID.String(), "not-a-chunk"}

	picked := pickPassages(chunks, near)

	total := 0
	var idx []int32
	for _, c := range picked {
		total += len(c.Text)
		idx = append(idx, c.ChunkIndex)
	}
	assert.LessOrEqual(t, total, passageBudgetRunes)
	assert.Equal(t, passageBudgetRunes, total, "the whole budget is used")
	head := int(float64(passageBudgetRunes)*headShare) / 1000
	for i := 0; i < head; i++ {
		assert.Contains(t, idx, int32(i), "the start of the document")
	}
	assert.Contains(t, idx, int32(30), "a passage the search found")
	assert.Contains(t, idx, int32(59), "the end of the document")
	assert.NotContains(t, idx, int32(head+5), "the middle that nothing asked for")
	for i := 1; i < len(idx); i++ {
		assert.Less(t, idx[i-1], idx[i], "in the order of the document")
	}
}

func TestNumbersAreKnownInTheirOtherWritings(t *testing.T) {
	known := numbersIn("The return is 1.41% and 0.50, the sample 1,234 firms, in 2019.")
	assert.Empty(t, unsupportedNumbers("Доходността е 1,41%, 0.5 и 1234 фирми през 2019 г.", known))
	assert.Equal(t, []string{"1.42", "2020"}, unsupportedNumbers("1.42% през 2020, пак 1.42", known))
	assert.Empty(t, unsupportedNumbers("1. Доходност 1.41%\n2) Пак", known), "the number of a numbered line is no number of the text")
}

func TestTheCheckKeepsTheLinesWithKnownNumbersOnly(t *testing.T) {
	known := numbersIn("1.41 and 2.5")
	c := checkAnswer("- Едно: 1.41.\n\n- Две: 9.9.\n- Три без число.\n* Четири: 2.5", known)
	assert.Equal(t, 3, c.facts)
	assert.Equal(t, 1, c.dropped)
	assert.Equal(t, "- Едно: 1.41.\n\n- Три без число.\n* Четири: 2.5", c.text)
}

func TestTheAnswerIsCleanedOfWhatAModelPutsAroundIt(t *testing.T) {
	assert.Equal(t, "- факт", cleanAnswer("<think>hm</think>\n```markdown\n- факт\n```"))
	assert.Equal(t, "- факт", cleanAnswer("  - факт  "))
}

func TestTheFileNameIsAlwaysOneTheFolderTakes(t *testing.T) {
	cases := map[string]string{
		"overnight-returns.pdf":     "overnight-returns - ключови факти.md",
		"Тихата река на зимата.pdf": "Тихата река на зимата - ключови факти.md",
		"a/b:c*?.pdf":               "a b c - ключови факти.md",
		"...pdf":                    "документ - ключови факти.md",
		"CON.pdf":                   "CON - ключови факти.md",
	}
	for doc, want := range cases {
		got := FileName(doc, "ключови факти", 0)
		assert.Equal(t, want, got, doc)
		assert.NoError(t, userfiles.ValidateName(got), doc)
	}
	long := FileName(strings.Repeat("дълго име ", 30)+".pdf", "ключови факти", 3)
	assert.NoError(t, userfiles.ValidateName(long))
	assert.True(t, strings.HasSuffix(long, " - ключови факти (4).md"), long)
}

func TestTheReportSaysWhatWasWrittenAndWhatNotAndWhy(t *testing.T) {
	p := &taskplan.Plan{Goal: "ключовите факти на всеки от 3 документа", Steps: []taskplan.Step{
		{ID: 1, Text: "a.pdf", Status: taskplan.StepDone, Evidence: "a - ключови факти.md (5 факта, 900 байта)"},
		{ID: 2, Text: "b.pdf", Status: taskplan.StepFailed, Note: "документът не се обработи навреме"},
		{ID: 3, Text: "c.pdf", Status: taskplan.StepDone, Evidence: "c - ключови факти.md (7 факта, 1200 байта)"},
	}}
	text := ReportText(p)
	require.True(t, strings.HasPrefix(text, "Готово: ключовите факти на всеки от 3 документа — 2 от 3."), text)
	assert.Contains(t, text, "• a - ключови факти.md (5 факта, 900 байта)\n• c - ключови факти.md")
	assert.Contains(t, text, "Не станаха:\n• b.pdf — документът не се обработи навреме")
	assert.Equal(t, "2 of 3 files written by the server", reportLog(p))
}
