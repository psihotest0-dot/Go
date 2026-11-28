package taskrun

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"go-monolith/internal/db"
	"go-monolith/internal/taskplan"
)

// A document a step expects (taskplan.Step.Expect): the link of a reference that the search found
// (agent/source_search.go). The search may give another work about it - a thesis or slides that cite
// it - so before its file is made, the start of the document must have the reference's title. On the
// real list of 2026-11-28, 8 of the 10 documents an early rule took were such other works.

const (
	// expectHeadRunes is how much of the start of a document is looked at for the title.
	expectHeadRunes = 5000
	// notExpected is the reason of a step whose document is another work.
	notExpected = "сваленият документ не е тази статия (заглавието ѝ не е в началото му) — пропускам го"
)

// isExpected tells whether the start of the document has the title: three quarters of its words, in
// any order (a title in a PDF is cut over lines, hyphenated, in capitals).
func isExpected(chunks []db.DocumentChunk, title string) bool {
	want := expectWords(title)
	if len(want) == 0 {
		return true
	}
	var b strings.Builder
	for _, c := range chunks {
		if c.ChunkType != "" && c.ChunkType != "raw_text" {
			continue
		}
		b.WriteString(" " + c.Text)
		if utf8.RuneCountInString(b.String()) >= expectHeadRunes {
			break
		}
	}
	head := " " + strings.Join(expectWords(b.String()), " ") + " "
	found := 0
	for _, w := range want {
		if strings.Contains(head, " "+w+" ") {
			found++
		}
	}
	return found*4 >= len(want)*3
}

// expectWords are the words of s, lower case, letters and digits only, from three letters on.
func expectWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if utf8.RuneCountInString(w) >= 3 {
			out = append(out, w)
		}
	}
	return out
}

// checkExpected fails the step whose document is not the work it expects; true when the step may go on.
func (r *Runner) checkExpected(ctx context.Context, p taskplan.RunPlan, step *taskplan.Step, file db.IngestedFile) bool {
	if step.Expect == "" || step.PartsDone > 0 {
		return true
	}
	chunks, err := r.db.Queries.GetDocumentChunksByFile(ctx, db.GetDocumentChunksByFileParams{FileID: file.ID, UserID: p.UserID})
	if err != nil || isExpected(chunks, step.Expect) {
		return true // a failed read is the work's to report
	}
	step.Status, step.Note = taskplan.StepFailed, notExpected
	return false
}
