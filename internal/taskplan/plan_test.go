package taskplan

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func steps(texts ...string) []StepInput {
	out := make([]StepInput, len(texts))
	for i, t := range texts {
		out[i] = StepInput{Text: t}
	}
	return out
}

func mustPlan(t *testing.T, inputs ...StepInput) *Plan {
	t.Helper()
	p, err := New("свали източниците", inputs)
	require.NoError(t, err)
	return p
}

func TestNewMakesAPlanOfPendingSteps(t *testing.T) {
	p, err := New("  свали източниците  ", []StepInput{{Text: " намери линковете "}, {Text: "добави ги", Tool: "add_to_library"}})

	require.NoError(t, err)
	assert.Equal(t, "свали източниците", p.Goal)
	assert.Equal(t, PlanActive, p.Status)
	require.Len(t, p.Steps, 2)
	assert.Equal(t, Step{ID: 1, Text: "намери линковете", Status: StepPending}, p.Steps[0])
	assert.Equal(t, Step{ID: 2, Text: "добави ги", Tool: "add_to_library", Status: StepPending}, p.Steps[1])
}

func TestNewRefusesWhatCannotBeMended(t *testing.T) {
	for name, tc := range map[string]struct {
		goal   string
		inputs []StepInput
		want   string
	}{
		"no goal":       {"  ", steps("а"), `missing "goal"`},
		"no steps":      {"цел", nil, `missing "steps"`},
		"an empty step": {"цел", steps("а", " "), "step 2 has no text"},
	} {
		_, err := New(tc.goal, tc.inputs)
		var invalid *InvalidError
		require.True(t, errors.As(err, &invalid), name)
		assert.Contains(t, invalid.Reason, tc.want, name)
	}
}

// The replay of 2026-11-21 lost 4 of 6 plans to refusals - 13 steps, a step of 361 characters, a
// tool the plan did not know - and the model went on in words with no plan. What can be mended
// is mended, and the model is told what was.
func TestNewMendsWhatIsOnlyTooMuch(t *testing.T) {
	long := strings.Repeat("д", MaxStepRunes+90)
	many := make([]StepInput, MaxSteps+4)
	for i := range many {
		many[i] = StepInput{Text: "стъпка"}
	}

	p, err := New(strings.Repeat("ц", MaxGoalRunes+50), many)
	require.NoError(t, err)
	assert.Len(t, p.Steps, MaxSteps, "the steps over the limit are left out")
	assert.LessOrEqual(t, len([]rune(p.Goal)), MaxGoalRunes)
	assert.True(t, strings.HasSuffix(p.Goal, "…"))
	require.Len(t, p.Warnings, 2)
	assert.Contains(t, p.Warnings[1], "only the first 25 of 29 steps were kept")

	p, err = New("цел", []StepInput{{Text: long}, {Text: "кратка"}, {Text: long, Tool: "rm_rf"}, {Text: "а", Tool: "list_documents"}})
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(p.Steps[0].Text)), MaxStepRunes)
	assert.True(t, strings.HasSuffix(p.Steps[0].Text, "…"))
	assert.Equal(t, "кратка", p.Steps[1].Text)
	assert.Empty(t, p.Steps[2].Tool, "an unknown tool is left out")
	assert.Equal(t, "list_documents", p.Steps[3].Tool, "a known one stays")
	require.Len(t, p.Warnings, 2)
	assert.Contains(t, p.Warnings[0], "steps 1, 3 were cut")
	assert.Contains(t, p.Warnings[1], `step 3: "rm_rf"`)
	assert.Contains(t, p.Warnings[1], "list_documents", "the tools it may name are listed")

	clean, err := New("цел", steps("а", "б"))
	require.NoError(t, err)
	assert.Empty(t, clean.Warnings, "nothing to say about a plan that is right")
}

func TestEveryToolOfAStepIsAcceptedAndNoOtherIs(t *testing.T) {
	for _, tool := range StepTools {
		p, err := New("цел", []StepInput{{Text: "а", Tool: tool}})
		require.NoError(t, err, tool)
		assert.Equal(t, tool, p.Steps[0].Tool)
		assert.Empty(t, p.Warnings, tool)
	}
	p, err := New("цел", []StepInput{{Text: "а", Tool: "append_file"}})
	require.NoError(t, err)
	assert.Empty(t, p.Steps[0].Tool, "a step names write_file; append_file is evidence for it, not a name")
	assert.Len(t, p.Warnings, 1)
}

func TestAStepWithAToolIsDoneOnlyOnTheEvidenceOfThatTool(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "свали ги", Tool: "add_to_library"}, StepInput{Text: "запиши", Tool: "write_file"})

	_, err := p.Update(1, StepDone, "")
	var invalid *InvalidError
	require.True(t, errors.As(err, &invalid))
	assert.True(t, strings.HasPrefix(invalid.Reason, "NOT done"), invalid.Reason)
	assert.Contains(t, invalid.Reason, "add_to_library")
	assert.Equal(t, StepPending, p.Steps[0].Status, "a refused step stays as it was")

	// A call of another tool is not the evidence.
	p.AddEvidence("web_fetch", "web_fetch x -> 3 KB")
	_, err = p.Update(1, StepDone, "")
	require.Error(t, err)

	p.AddEvidence("add_to_library", "add_to_library: 13 of 21 queued")
	out, err := p.Update(1, StepDone, "13 са добавени")
	require.NoError(t, err)
	assert.Equal(t, StepDone, p.Steps[0].Status)
	assert.Equal(t, "add_to_library: 13 of 21 queued", p.Steps[0].Evidence)
	assert.Equal(t, "13 са добавени", p.Steps[0].Note)
	assert.False(t, out.Finished)
	assert.Contains(t, out.Message, "Next is step 2")
	assert.Contains(t, out.Message, "Use write_file")
}

func TestACallIsEvidenceForOneStepOnly(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "а", Tool: "web_fetch"}, StepInput{Text: "б", Tool: "web_fetch"})
	p.AddEvidence("web_fetch", "web_fetch one")

	_, err := p.Update(1, StepDone, "")
	require.NoError(t, err)
	_, err = p.Update(2, StepDone, "")
	require.Error(t, err, "the one call was used by step 1")

	p.AddEvidence("web_fetch", "web_fetch two")
	_, err = p.Update(2, StepDone, "")
	require.NoError(t, err)
	assert.Equal(t, "web_fetch two", p.Steps[1].Evidence)
	assert.True(t, p.Ledger[0].Used && p.Ledger[1].Used)
}

func TestTheEvidenceIsTakenInTheOrderItCame(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "а", Tool: "web_fetch"})
	p.AddEvidence("web_fetch", "first")
	p.AddEvidence("web_fetch", "second")

	_, err := p.Update(1, StepDone, "")

	require.NoError(t, err)
	assert.Equal(t, "first", p.Steps[0].Evidence)
	assert.False(t, p.Ledger[1].Used)
}

func TestWritingAndAppendingBothCountForAWriteStep(t *testing.T) {
	for _, tool := range []string{"write_file", "append_file"} {
		p := mustPlan(t, StepInput{Text: "запиши", Tool: "write_file"})
		p.AddEvidence(tool, tool+" a.md: saved")
		_, err := p.Update(1, StepDone, "")
		require.NoError(t, err, tool)
	}
	// Only the other way round does not hold: reading is not writing.
	p := mustPlan(t, StepInput{Text: "запиши", Tool: "write_file"})
	p.AddEvidence("read_file", "read_file a.md")
	_, err := p.Update(1, StepDone, "")
	require.Error(t, err)
}

func TestAStepThatNeedsNoToolIsDoneOnTheWordOfTheModel(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "реши кои източници са най-важни", Tool: ToolNone})

	out, err := p.Update(1, StepDone, "избрах три")

	require.NoError(t, err)
	assert.Equal(t, StepDone, p.Steps[0].Status)
	assert.Empty(t, p.Steps[0].Evidence)
	assert.True(t, out.Finished)
}

func TestFailedAndSkippedNeedTheReason(t *testing.T) {
	for _, status := range []string{StepFailed, StepSkipped} {
		p := mustPlan(t, StepInput{Text: "а", Tool: "web_fetch"}, StepInput{Text: "б", Tool: ToolNone})

		_, err := p.Update(1, status, "  ")
		var invalid *InvalidError
		require.True(t, errors.As(err, &invalid), status)
		assert.Contains(t, invalid.Reason, "say why step 1 is "+status)
		assert.Equal(t, StepPending, p.Steps[0].Status)

		out, err := p.Update(1, status, "линкът дава 404")
		require.NoError(t, err, status)
		assert.Equal(t, status, p.Steps[0].Status)
		assert.Equal(t, "линкът дава 404", p.Steps[0].Note)
		assert.Contains(t, out.Message, "Next is step 2", "the plan goes on")
	}
}

func TestADoneStepCannotBeFailedAndAFinishedPlanIsOver(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "а", Tool: ToolNone}, StepInput{Text: "б", Tool: ToolNone})
	_, err := p.Update(1, StepDone, "")
	require.NoError(t, err)

	_, err = p.Update(1, StepFailed, "аз реших")
	require.Error(t, err)
	_, err = p.Update(1, StepDoing, "")
	require.Error(t, err)
	out, err := p.Update(1, StepDone, "")
	require.NoError(t, err, "done again is not an error")
	assert.Contains(t, out.Message, "done already")

	out, err = p.Update(2, StepSkipped, "не трябва")
	require.NoError(t, err)
	assert.True(t, out.Finished)
	assert.Equal(t, PlanDone, p.Status)
	assert.Contains(t, out.Message, "Every step is finished")

	_, err = p.Update(2, StepDone, "")
	var invalid *InvalidError
	require.True(t, errors.As(err, &invalid))
	assert.Contains(t, invalid.Reason, "this plan is over")
}

func TestAnUpdateOfNothingIsRefused(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "а", Tool: ToolNone})

	_, err := p.Update(7, StepDone, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "there is no step 7")
	_, err = p.Update(0, StepDone, "")
	require.Error(t, err)
	_, err = p.Update(1, "finished", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not one of doing, done, failed, skipped")
	out, err := p.Update(1, StepDoing, "")
	require.NoError(t, err)
	assert.Equal(t, StepDoing, p.Steps[0].Status)
	assert.Contains(t, out.Message, "in progress")
}

func TestTheLedgerKeepsTheNewestAndDropsTheUsedFirst(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "а", Tool: "web_fetch"})
	for i := 0; i < MaxLedger; i++ {
		p.AddEvidence("web_search", "x")
	}
	p.Ledger[3].Used = true

	p.AddEvidence("web_fetch", "the newest")

	require.Len(t, p.Ledger, MaxLedger)
	assert.Equal(t, "the newest", p.Ledger[len(p.Ledger)-1].Gist)
	assert.Equal(t, 41, p.Ledger[len(p.Ledger)-1].N, "the numbers keep growing")
	p.AddEvidence("web_fetch", "the one after")
	assert.Equal(t, 42, p.Ledger[len(p.Ledger)-1].N, "and never repeat one that was dropped")
	for _, e := range p.Ledger {
		assert.False(t, e.Used, "the used entry went first")
	}

	// With nothing used the oldest goes.
	q := mustPlan(t, StepInput{Text: "а"})
	for i := 0; i < MaxLedger+1; i++ {
		q.AddEvidence("web_search", "x")
	}
	require.Len(t, q.Ledger, MaxLedger)
	assert.Equal(t, 2, q.Ledger[0].N)
}

func TestTheEvidenceLineIsCut(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "а"})

	p.AddEvidence("web_fetch", strings.Repeat("дълга ", 100))

	assert.LessOrEqual(t, len([]rune(p.Ledger[0].Gist)), MaxEvidenceRunes)
	assert.True(t, strings.HasSuffix(p.Ledger[0].Gist, "…"))
}

func TestRenderShowsWhereTheWorkIs(t *testing.T) {
	p := mustPlan(t,
		StepInput{Text: "намери линковете в ревюто", Tool: ToolNone},
		StepInput{Text: "добави ги в библиотеката", Tool: "add_to_library"},
		StepInput{Text: "запиши списъка", Tool: "write_file"},
		StepInput{Text: "сравни резултатите", Tool: ToolNone},
		StepInput{Text: "провери PDF-а", Tool: ToolNone},
	)
	_, err := p.Update(1, StepDone, "")
	require.NoError(t, err)
	p.AddEvidence("add_to_library", "add_to_library: 13 of 21 queued")
	_, err = p.Update(2, StepDone, "")
	require.NoError(t, err)
	_, err = p.Update(5, StepFailed, "линкът дава 404")
	require.NoError(t, err)

	note := Render(p)

	assert.True(t, strings.HasPrefix(note, "\n\n[Plan of the task, kept by the system - not written by the user]\nGoal: свали източниците\n"))
	assert.Contains(t, note, "1. done - намери линковете в ревюто\n")
	assert.Contains(t, note, "2. done - добави ги в библиотеката (add_to_library: 13 of 21 queued)\n")
	assert.Contains(t, note, "3. NEXT - запиши списъка [tool: write_file]\n")
	assert.Contains(t, note, "4. todo - сравни резултатите\n")
	assert.Contains(t, note, "5. failed - провери PDF-а (линкът дава 404)\n")
	assert.Contains(t, note, "do the NEXT step now with its tool, then call plan_update")
	assert.Contains(t, note, "Never write what a source says if you did not open it")
	assert.True(t, strings.HasSuffix(note, "answer from this list.]"))
	// Only one step is NEXT, and a plan of this size costs a few hundred tokens.
	assert.Equal(t, 1, strings.Count(note, " NEXT - "))
	assert.Less(t, len(note)/4, 450, "about %d tokens", len(note)/4)
}

func TestRenderIsShortWhateverThePlanHolds(t *testing.T) {
	long := strings.Repeat("много дълъг текст ", 20)
	inputs := make([]StepInput, MaxSteps)
	for i := range inputs {
		inputs[i] = StepInput{Text: string([]rune(long)[:MaxStepRunes*2/3]), Tool: "web_fetch"}
	}
	p := mustPlan(t, inputs...)
	p.Goal = long
	p.Steps[0].Status, p.Steps[0].Evidence = StepDone, long
	p.Steps[1].Status, p.Steps[1].Note = StepFailed, long

	note := Render(p)

	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(line, "How to work") {
			continue
		}
		if strings.HasPrefix(line, "Goal:") {
			assert.LessOrEqual(t, len([]rune(line)), MaxGoalRunes+7)
			continue
		}
		assert.LessOrEqual(t, len([]rune(line)), renderStepRunes+renderEvidenceRunes+50, line)
	}
	assert.Less(t, len(note)/4, 1300, "about %d tokens at the worst", len(note)/4)
}

func TestRenderOfAStepInProgressAndOfAFinishedPlan(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "а", Tool: "web_fetch"}, StepInput{Text: "б", Tool: ToolNone})
	_, err := p.Update(1, StepDoing, "")
	require.NoError(t, err)

	note := Render(p)

	assert.Contains(t, note, "1. doing - а [tool: web_fetch]")
	assert.Contains(t, note, "2. todo - б", "while a step is in progress no other step is NEXT")
	assert.NotContains(t, note, " NEXT - ")
}

// A step that names no tool at all is not a step that needs none: it is done when some tool
// has worked since, which a plan of two vague steps ("process the sources", "write the files")
// done on words alone did in the replay of 2026-11-21 while every file was invented.
func TestAStepThatNamesNoToolNeedsSomeToolToHaveWorked(t *testing.T) {
	p := mustPlan(t, StepInput{Text: "обработи източниците"}, StepInput{Text: "запиши файловете"})

	_, err := p.Update(1, StepDone, "")
	var invalid *InvalidError
	require.True(t, errors.As(err, &invalid))
	assert.Contains(t, invalid.Reason, "NOT done: no successful call of any tool is on record")

	p.AddEvidence("list_documents", "list_documents -> 13 files")
	_, err = p.Update(1, StepDone, "")
	require.NoError(t, err)
	assert.Equal(t, "list_documents -> 13 files", p.Steps[0].Evidence)

	_, err = p.Update(2, StepDone, "")
	require.Error(t, err, "the one call was used by step 1")
	p.AddEvidence("write_file", "write_file a.md: saved")
	_, err = p.Update(2, StepDone, "")
	require.NoError(t, err)
}

func manySteps(n int) *Plan {
	inputs := make([]StepInput, n)
	for i := range inputs {
		inputs[i] = StepInput{Text: fmt.Sprintf("източник номер %d", i+1), Tool: ToolNone}
	}
	p, _ := New("файл за всеки източник", inputs)
	return p
}

// A plan of many steps is shown in a window, so that it costs the same whatever its size.
func TestAPlanOfManyStepsIsShownInAWindow(t *testing.T) {
	p := manySteps(13)
	for i := 1; i <= 5; i++ {
		_, err := p.Update(i, StepDone, "")
		require.NoError(t, err)
	}
	_, err := p.Update(7, StepFailed, "няма текст")
	require.NoError(t, err)
	_, err = p.Update(8, StepSkipped, "повтаря петия")
	require.NoError(t, err)

	note := Render(p)

	assert.Contains(t, note, "Progress: 5 of 13 steps done, 1 failed, 1 skipped.")
	assert.Contains(t, note, "5. done - източник номер 5\n", "the last one done")
	assert.NotContains(t, note, "1. done", "the earlier ones are not listed")
	assert.NotContains(t, note, "4. done")
	assert.Contains(t, note, "7. failed - източник номер 7 (няма текст)")
	assert.Contains(t, note, "8. skipped - източник номер 8 (повтаря петия)")
	assert.Contains(t, note, "6. NEXT - източник номер 6")
	assert.Contains(t, note, "9. todo - източник номер 9")
	assert.Contains(t, note, "10. todo - източник номер 10")
	assert.NotContains(t, note, "11. todo", "only the next steps are listed")
	assert.Contains(t, note, "(3 more steps to do after these)")
	assert.Equal(t, 1, strings.Count(note, " NEXT - "))
}

func TestAPlanOfManyStepsCostsTheSameWhateverItsSize(t *testing.T) {
	small, large := Render(manySteps(9)), Render(manySteps(MaxSteps))

	assert.Less(t, len(large)/4, 480, "about %d tokens", len(large)/4)
	assert.InDelta(t, len(small), len(large), 120, "the window is the same size")
	// A plan up to the limit of the full list is listed in full.
	full := Render(manySteps(renderAllSteps))
	assert.Contains(t, full, "1. NEXT - източник номер 1")
	assert.Contains(t, full, "8. todo - източник номер 8")
	assert.NotContains(t, full, "Progress:")
}
