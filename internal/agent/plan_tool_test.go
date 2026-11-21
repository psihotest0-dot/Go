package agent

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/taskplan"
)

func planEnv(t *testing.T) (*toolEnv, *taskplan.Store) {
	t.Helper()
	e := newToolEnv(t)
	store := taskplan.NewStore(e.db.Queries)
	e.reg.WithPlans(store)
	return e, store
}

func planOrchestrator(mock *llm.MockLLM, e *toolEnv, store *taskplan.Store) *Orchestrator {
	return &Orchestrator{llmClient: mock, tools: e.reg, groups: []toolGroup{planGroup()}, plans: store}
}

func toolMessages(mock *llm.MockLLM) []string {
	var out []string
	for _, req := range mock.Requests {
		for _, m := range req.Messages {
			if m.Role == openai.ChatMessageRoleTool {
				out = append(out, m.Content)
			}
		}
	}
	return out
}

func TestThePlanToolsAreOneStubAndTwoTools(t *testing.T) {
	group := planGroup()

	assert.Equal(t, planStubTool, group.stubName())
	assert.Equal(t, []string{planSetTool, planUpdateTool}, toolNames(group.tools))
	assert.Equal(t, "plan", planStubTool, "the name the model has been measured with")

	var set, update *openai.FunctionDefinition
	for _, tool := range group.tools {
		switch tool.Function.Name {
		case planSetTool:
			set = tool.Function
		case planUpdateTool:
			update = tool.Function
		}
	}
	params := set.Parameters.(map[string]any)
	assert.Equal(t, []string{"goal", "steps"}, params["required"])
	step := params["properties"].(map[string]any)["steps"].(map[string]any)["items"].(map[string]any)
	assert.Equal(t, []string{"text"}, step["required"], "the tool of a step is optional")
	assert.Equal(t, taskplan.StepTools, step["properties"].(map[string]any)["tool"].(map[string]any)["enum"])
	assert.Equal(t, []string{"step", "status"}, update.Parameters.(map[string]any)["required"])
	assert.Equal(t, []string{"doing", "done", "failed", "skipped"},
		update.Parameters.(map[string]any)["properties"].(map[string]any)["status"].(map[string]any)["enum"])
	// What a turn pays for the stub, in every request.
	assert.Less(t, planStubTokens(), 130)
}

func TestWithoutPlansTheToolsAreUnknown(t *testing.T) {
	e := newToolEnv(t)
	ctx, _ := e.turn("s", false)

	for _, name := range []string{planStubTool, planSetTool, planUpdateTool} {
		_, err := e.reg.ExecuteTool(ctx, name, map[string]interface{}{})
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "unknown tool", name)
	}
}

func TestPlanSetMakesThePlanAndTellsWhatToDoFirst(t *testing.T) {
	e, store := planEnv(t)
	ctx, turn := e.turn("s", false)

	res, err := e.reg.ExecuteTool(ctx, planSetTool, map[string]interface{}{
		"goal": "свали източниците",
		"steps": []interface{}{
			map[string]interface{}{"text": "добави линковете", "tool": "add_to_library"},
			map[string]interface{}{"text": "кажи какво е станало"},
		},
	})

	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, "ok", m["status"])
	assert.Equal(t, 2, m["steps"])
	assert.Contains(t, m["message"], "Do step 1 now with add_to_library, then call plan_update")
	plan, err := store.Active(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "свали източниците", plan.Goal)
	assert.Equal(t, planActiveRounds, turn.extraRounds(), "a turn that made a plan has the rounds of a plan")
}

func TestPlanSetUnderstandsStepsWrittenAsPlainText(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)

	_, err := e.reg.ExecuteTool(ctx, planSetTool, map[string]interface{}{
		"goal": "цел", "steps": []interface{}{"първа стъпка", "втора стъпка"},
	})

	require.NoError(t, err)
	plan, err := store.Active(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.Len(t, plan.Steps, 2)
	assert.Equal(t, "втора стъпка", plan.Steps[1].Text)
}

func TestPlanSetRefusalsAreCallsTheModelCanFix(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	for name, args := range map[string]map[string]interface{}{
		"no goal":        {"steps": []interface{}{"а"}},
		"no steps":       {"goal": "цел"},
		"empty steps":    {"goal": "цел", "steps": []interface{}{}},
		"a step of 7":    {"goal": "цел", "steps": []interface{}{7}},
		"a step no text": {"goal": "цел", "steps": []interface{}{map[string]interface{}{"tool": "web_fetch"}}},
	} {
		_, err := e.reg.ExecuteTool(ctx, planSetTool, args)
		require.Error(t, err, name)
		var argErr *toolArgumentError
		assert.ErrorAs(t, err, &argErr, name+": the model is told, the server did not fail")
	}
	plan, err := store.Active(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Nil(t, plan, "nothing was made")
}

func TestPlanUpdateAcceptsDoneOnlyAfterTheToolSucceeded(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	_, err := store.Set(ctx, e.userID, "s", "цел", []taskplan.StepInput{{Text: "свали", Tool: "add_to_library"}})
	require.NoError(t, err)

	_, err = e.reg.ExecuteTool(ctx, planUpdateTool, map[string]interface{}{"step": float64(1), "status": "done"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NOT done")
	var argErr *toolArgumentError
	assert.ErrorAs(t, err, &argErr)

	_, err = store.Record(ctx, e.userID, "s", "add_to_library", "add_to_library: 2 of 2 queued")
	require.NoError(t, err)
	res, err := e.reg.ExecuteTool(ctx, planUpdateTool, map[string]interface{}{"step": float64(1), "status": "DONE"})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, true, m["finished"])
	assert.Contains(t, m["message"], "Every step is finished")
}

func TestPlanUpdateNeedsAStepAndAStatus(t *testing.T) {
	e, _ := planEnv(t)
	ctx, _ := e.turn("s", false)

	_, err := e.reg.ExecuteTool(ctx, planUpdateTool, map[string]interface{}{"status": "done"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"step"`)
	_, err = e.reg.ExecuteTool(ctx, planUpdateTool, map[string]interface{}{"step": float64(1)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"status"`)
	_, err = e.reg.ExecuteTool(ctx, planUpdateTool, map[string]interface{}{"step": float64(1), "status": "done"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no plan in progress")
}

func TestWhatATurnReadsOrWritesIsEvidenceOnlyWhenItWorked(t *testing.T) {
	rows := []addRow{{Status: addQueued}, {Status: addRefused}, {Status: addDuplicate}}
	for name, tc := range map[string]struct {
		tool string
		args map[string]interface{}
		res  interface{}
		size int
		ok   bool
		gist string
	}{
		"a page":                {"web_fetch", map[string]interface{}{"url": "https://nber.org/papers/w1"}, map[string]interface{}{}, 40000, true, "web_fetch https://nber.org/papers/w1 -> 39.1 KB"},
		"a page that is short":  {"web_fetch", map[string]interface{}{"url": "https://nber.org/papers/w1"}, map[string]interface{}{}, 1300, true, "(short: a summary?)"},
		"a search":              {"web_search", map[string]interface{}{"query": "overnight drift"}, map[string]interface{}{}, 17000, true, `web_search "overnight drift"`},
		"a search of nothing":   {"web_search", map[string]interface{}{"query": "x"}, map[string]interface{}{}, 40, false, ""},
		"links added":           {addToLibraryTool, nil, map[string]interface{}{"results": rows, "summary": "1 of 3 queued, 1 already in the library, 1 not added"}, 900, true, "add_to_library: 1 of 3 queued"},
		"no link added":         {addToLibraryTool, nil, map[string]interface{}{"results": []addRow{{Status: addRefused}}, "summary": "0 of 1 queued"}, 300, false, ""},
		"passages":              {searchDocumentsTool, map[string]interface{}{"query": "дрейф"}, map[string]interface{}{}, 7800, true, `search_documents "дрейф" -> 7.6 KB`},
		"no passages":           {searchDocumentsTool, map[string]interface{}{"query": "дрейф"}, map[string]interface{}{}, 20, false, ""},
		"a file read":           {readFileTool, map[string]interface{}{"name": "a.md"}, map[string]interface{}{"status": "success"}, 2048, true, "read_file a.md -> 2.0 KB"},
		"a file not there":      {readFileTool, map[string]interface{}{"name": "a.md"}, map[string]interface{}{"status": "not_found"}, 80, false, ""},
		"a file saved":          {writeFileTool, map[string]interface{}{"name": "a.md"}, map[string]interface{}{"status": "saved"}, 100, true, "write_file a.md: saved"},
		"a file appended":       {appendFileTool, map[string]interface{}{"name": "a.md"}, map[string]interface{}{"status": "appended"}, 100, true, "append_file a.md: appended"},
		"a write that waits":    {writeFileTool, map[string]interface{}{"name": "a.md"}, map[string]interface{}{"status": "pending_approval"}, 300, true, "waiting for the user's confirmation"},
		"a write not found":     {appendFileTool, map[string]interface{}{"name": "a.md"}, map[string]interface{}{"status": "not_found"}, 100, false, ""},
		"a tool of no evidence": {"create_memory", nil, map[string]interface{}{"status": "success"}, 100, false, ""},
		"the plan itself":       {planUpdateTool, nil, map[string]interface{}{"status": "ok"}, 100, false, ""},
	} {
		gist, ok := planEvidence(tc.tool, tc.args, tc.res, tc.size)
		assert.Equal(t, tc.ok, ok, name)
		if tc.ok {
			assert.Contains(t, gist, tc.gist, name)
		}
	}
}

// The whole of it, with the real registry, the real database and the real tools but a scripted
// model: it makes a plan, opens a page, and reports the step done; the server wrote the page in
// the ledger and takes it as the evidence of the step.
func TestAStepIsDoneOnWhatTheToolDid(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", planStubTool, `{}`)},
		{toolCallChunk(0, "c2", planSetTool, `{"goal":"прочети статията","steps":[{"text":"отвори страницата","tool":"web_fetch"},{"text":"обобщи","tool":"none"}]}`)},
		{toolCallChunk(0, "c3", "web_fetch", `{"url":"https://example.com/paper"}`)},
		{toolCallChunk(0, "c4", planUpdateTool, `{"step":1,"status":"done"}`)},
		{toolCallChunk(0, "c5", planUpdateTool, `{"step":2,"status":"done","note":"обобщено"}`)},
		{textChunk("Готово.")},
	}}
	o := planOrchestrator(mock, e, store)

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "прочети статията и я обобщи"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Готово.", content)
	assert.Equal(t, []string{planStubTool, planSetTool, "web_fetch", planUpdateTool, planUpdateTool}, toolCallNames(executed))
	plan, err := store.Latest(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Equal(t, taskplan.PlanDone, plan.Status)
	assert.Equal(t, taskplan.StepDone, plan.Steps[0].Status)
	assert.Contains(t, plan.Steps[0].Evidence, "web_fetch https://example.com/paper", "what the tool did, not what the model said")
	assert.Equal(t, "обобщено", plan.Steps[1].Note)
	require.Len(t, plan.Ledger, 1)
	assert.True(t, plan.Ledger[0].Used)
}

// "Done" said before the tool ran is refused, the model is told so in the result, and the step
// is done when the tool has worked. The turn has the rounds this takes.
func TestAStepClaimedDoneBeforeTheToolRanIsRefused(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", planStubTool, `{}`)},
		{toolCallChunk(0, "c2", planSetTool, `{"goal":"прочети","steps":[{"text":"отвори страницата","tool":"web_fetch"}]}`)},
		{toolCallChunk(0, "c3", planUpdateTool, `{"step":1,"status":"done"}`)},
		{toolCallChunk(0, "c4", "web_fetch", `{"url":"https://example.com/paper"}`)},
		{toolCallChunk(0, "c5", planUpdateTool, `{"step":1,"status":"done"}`)},
		{textChunk("Отворих страницата.")},
	}}
	o := planOrchestrator(mock, e, store)

	content, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "прочети статията"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Отворих страницата.", content)
	var refused bool
	for _, m := range toolMessages(mock) {
		if strings.Contains(m, "NOT done: no successful call of web_fetch") {
			refused = true
		}
	}
	assert.True(t, refused, "the model was told that the step is not done")
	plan, err := store.Latest(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Equal(t, taskplan.StepDone, plan.Steps[0].Status, "it is done once the page was opened")
}

func TestAToolOfAConversationWithoutAPlanLeavesNothing(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", "web_fetch", `{"url":"https://example.com/paper"}`)},
		{textChunk("Отворих я.")},
	}}
	o := planOrchestrator(mock, e, store)

	_, _, _, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "отвори я"}}, o.turnTools(), nil)

	require.NoError(t, err)
	plan, err := store.Latest(context.Background(), e.userID, "s")
	require.NoError(t, err)
	assert.Nil(t, plan)
}

// The plan in progress is put next to the last message of the person, the tools of the plan are
// on offer at once and no question is asked at the end; a conversation without a plan has the
// stub, as it had.
func TestATurnOfAConversationWithAPlanSeesThePlan(t *testing.T) {
	e := newAttachmentEnv(t)
	store := taskplan.NewStore(e.db.Queries)
	e.reg.WithPlans(store)
	o := orchestratorWith(t, e, answeringModel()).WithTaskPlans(store)
	_, err := store.Set(context.Background(), e.userID, "p1", "свали източниците", []taskplan.StepInput{
		{Text: "добави линковете", Tool: "add_to_library"}, {Text: "кажи резултата"},
	})
	require.NoError(t, err)
	storeReplies(t, e, "p1", 3, "продължи") // the 4th reply: the one that would end with a question
	storeReplies(t, e, "p2", 3, "продължи")

	with, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "p1", Content: "продължи"}, false)
	require.NoError(t, err)
	without, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "p2", Content: "продължи"}, false)
	require.NoError(t, err)

	note := lastMessage(t, with)
	assert.True(t, strings.HasPrefix(note, "продължи\n\n[Plan of the task, kept by the system"), note)
	assert.Contains(t, note, "1. NEXT - добави линковете [tool: add_to_library]")
	assert.NotContains(t, note, "[Note for the answer", "a task has no question to end with")
	names := toolNames(with.tools)
	assert.Contains(t, names, planSetTool)
	assert.Contains(t, names, planUpdateTool)
	assert.NotContains(t, names, planStubTool, "the tools stand in the place of the stub")
	assert.Equal(t, planActiveRounds, with.turn.extraRounds())
	for _, m := range with.messages[1 : len(with.messages)-1] {
		assert.NotContains(t, m.Content, "[Plan of the task", "only the last message carries the plan")
	}

	assert.Equal(t, "продължи"+closingNoteQuestion, lastMessage(t, without), "another conversation is as it was")
	assert.Contains(t, toolNames(without.tools), planStubTool)
	assert.NotContains(t, toolNames(without.tools), planSetTool)
	assert.Zero(t, without.turn.extraRounds())
}

func TestThePlanNoteIsNotStored(t *testing.T) {
	e := newAttachmentEnv(t)
	store := taskplan.NewStore(e.db.Queries)
	e.reg.WithPlans(store)
	o := orchestratorWith(t, e, answeringModel()).WithTaskPlans(store)
	_, err := store.Set(context.Background(), e.userID, "p3", "цел", []taskplan.StepInput{{Text: "а"}})
	require.NoError(t, err)
	storeReplies(t, e, "p3", 1, "продължи")

	_, err = o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "p3", Content: "продължи"}, false)
	require.NoError(t, err)

	rows, err := e.db.Queries.GetMessagesBySession(context.Background(), db.GetMessagesBySessionParams{UserID: e.userID, SessionID: "p3", Limit: 50})
	require.NoError(t, err)
	for _, m := range rows {
		assert.NotContains(t, m.Content, "[Plan of the task", "the history stays as the person wrote it")
	}
}

func TestWithoutThePlanFeatureATurnIsAsItWas(t *testing.T) {
	e := newAttachmentEnv(t)
	o := orchestratorWith(t, e, answeringModel())
	storeReplies(t, e, "p4", 0, "здравей")

	prep, err := o.prepareTurn(context.Background(), e.userID, ChatRequest{SessionID: "p4", Content: "здравей"}, false)

	require.NoError(t, err)
	assert.Equal(t, "здравей"+closingNoteStatement, lastMessage(t, prep))
	assert.NotContains(t, toolNames(prep.tools), planStubTool)
	assert.Zero(t, prep.turn.extraRounds())
}

func TestTheRoundsOfAPlanReplaceTheRoundsOfTheGroups(t *testing.T) {
	// Seven tools in a row, then the answer: more than the 4 rounds a group gives, fewer than the 6 of a plan.
	e, store := planEnv(t)
	ctx, turn := e.turn("s", false)
	turn.markPlanActive()
	script := [][]openai.ChatCompletionStreamResponse{}
	for i := 0; i < 5; i++ {
		script = append(script, []openai.ChatCompletionStreamResponse{toolCallChunk(0, "c", "web_fetch", `{"url":"https://example.com/paper"}`)})
	}
	script = append(script, []openai.ChatCompletionStreamResponse{textChunk("Сега е готово.")})
	mock := &llm.MockLLM{StreamScript: script}
	o := planOrchestrator(mock, e, store)

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "продължи"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Len(t, executed, 5)
	assert.Equal(t, "Сега е готово.", content)
}

// The stub of the plan is in every request of a conversation when the plan is on, so the room
// for it comes out of the history and the passages, as the stub of the file tools does.
func TestThePlanReservesRoomForItsStub(t *testing.T) {
	cfg := &config.Config{LLMContextTokens: 16384}
	without := newBudgets(cfg)
	cfg.TaskPlanEnabled = true
	with := newBudgets(cfg)

	assert.Zero(t, without.extraFixedTokens)
	assert.Equal(t, planStubTokens(), with.extraFixedTokens)
	cfg.FilesEnabled = true
	both := newBudgets(cfg)
	assert.Equal(t, filesStubTokens+planStubTokens(), both.extraFixedTokens, "the two stubs add up")
	assert.LessOrEqual(t, both.worstCaseTokens(), 16384)
}

func TestAsksForManySteps(t *testing.T) {
	for message, want := range map[string]bool{
		"сега за всеки от добавените източници направи отделен файл с ключовите му факти, за да ги ползвам при стратегии": true,
		"прочети всички статии и ги обобщи":                     true,
		"направи файл за всяка книга от библиотеката":           true,
		"Make a separate file for each paper and summarize it":  true,
		"извлечи информацията от всички документи един по един": true,
		"запиши поотделно всеки раздел от ревюто във файл":      true,
		"здравей": false,
		"какво мислиш за първия източник?":        false, // an opinion, one item
		"запиши го за всеки случай във файла":     false, // "just in case"
		"всички са добре, благодаря":              false, // no work, no set of items
		"направи ми файл с идеи":                  false, // one file
		"искам да изтеглиш всички тези източници": true,
		"продължи": false,
		"успя ли?": false,
		"":         false,
		"направи файл за всички файлове: " + strings.Repeat("много дълго съобщение ", 40): false, // a story or the material itself
	} {
		assert.Equal(t, want, asksForManySteps(message), message)
	}
}

// A message that asks for work on every item makes the first round call the stub of the plan,
// where there is one on offer: the model does not make a plan on its own (replay of 2026-11-21).
func TestWorkOnEveryItemForcesThePlan(t *testing.T) {
	const message = "за всеки от източниците направи отделен файл"
	withPlan := append(toolDefinitions(), planStubDefinition())

	assert.Equal(t, planStubTool, forcedFirstTool(withPlan, message))
	assert.Empty(t, forcedFirstTool(toolDefinitions(), message), "no plan on offer, nothing is forced")
	assert.Empty(t, forcedFirstTool(withPlan, "здравей"), "a message that is not such a task is left to the model")
	// The plan comes before a question to the library, which is also a question about the files.
	withLibrary := append(withPlan, openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: listDocumentsTool}})
	assert.Equal(t, planStubTool, forcedFirstTool(withLibrary, "какви книги има в библиотеката? направи файл за всяка"))
}

// The stub of the plan is forced in the first round (work on every item), and the round after it
// is the one that must use the tools the stub unlocked. They share the rule "the first round
// offers one tool and requires a call", and the second must not be taken for the first: the
// first replay of the plan (2026-11-21) forced the stub twice, offered the tools of the plan to
// nobody, and the model went on in words with no plan made.
func TestAfterTheForcedStubThePlanToolsAreOffered(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	mock := &llm.MockLLM{StreamScript: [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", planStubTool, `{}`)},
		{toolCallChunk(0, "c2", planSetTool, `{"goal":"файл за всеки източник","steps":[{"text":"направи файла за първия"},{"text":"направи файла за втория"}]}`)},
		{textChunk("Направих плана.")},
	}}
	o := planOrchestrator(mock, e, store)

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "за всеки от източниците направи отделен файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Направих плана.", content)
	assert.Equal(t, []string{planStubTool, planSetTool}, toolCallNames(executed))
	require.Len(t, mock.Requests, 3)
	first, second := mock.Requests[0], mock.Requests[1]
	assert.Equal(t, []string{planStubTool}, toolNames(first.Tools), "the first round offers the stub alone")
	assert.Equal(t, "required", first.ToolChoice)
	assert.ElementsMatch(t, []string{planSetTool, planUpdateTool}, toolNames(second.Tools), "the second offers the tools the stub unlocked")
	assert.Equal(t, "required", second.ToolChoice)
	assert.Nil(t, mock.Requests[2].ToolChoice, "then the model is free")
	plan, err := store.Active(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Len(t, plan.Steps, 2)
}

// A plan the model wrote loosely is mended, not refused (the replay lost 4 of 6 plans to
// refusals), and the answer to the model says what was mended.
func TestPlanSetMendsAPlanAndTellsWhatItMended(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	long := strings.Repeat("д", taskplan.MaxStepRunes+100)

	res, err := e.reg.ExecuteTool(ctx, planSetTool, map[string]interface{}{
		"goal": "цел",
		"steps": []interface{}{
			map[string]interface{}{"text": long, "tool": "rm_rf"},
			map[string]interface{}{"text": "втора", "tool": "list_documents"},
		},
	})

	require.NoError(t, err)
	m := res.(map[string]interface{})
	warnings, _ := m["warnings"].([]string)
	require.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], "were cut")
	assert.Contains(t, warnings[1], `"rm_rf"`)
	plan, err := store.Active(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Empty(t, plan.Steps[0].Tool)
	assert.Equal(t, "list_documents", plan.Steps[1].Tool)

	clean, err := e.reg.ExecuteTool(ctx, planSetTool, map[string]interface{}{"goal": "цел", "steps": []interface{}{"а", "б"}})
	require.NoError(t, err)
	assert.NotContains(t, clean.(map[string]interface{}), "warnings")
}

func TestTheLibraryListIsEvidenceWhenItHasFiles(t *testing.T) {
	gist, ok := planEvidence(listDocumentsTool, nil, map[string]interface{}{"status": "success", "count": 13}, 900)
	assert.True(t, ok)
	assert.Equal(t, "list_documents -> 13 files", gist)
	_, ok = planEvidence(listDocumentsTool, nil, map[string]interface{}{"status": "success", "count": 0}, 60)
	assert.False(t, ok, "an empty library proves nothing")
}

// A plan gives a turn 6 rounds for tools; the rounds of the group that was unlocked in the same
// turn are not added to them (that would be 8).
func TestAPlanTurnHasSixRoundsForToolsWhateverWasUnlocked(t *testing.T) {
	e, store := planEnv(t)
	ctx, _ := e.turn("s", false)
	script := [][]openai.ChatCompletionStreamResponse{
		{toolCallChunk(0, "c1", planStubTool, `{}`)},
		{toolCallChunk(0, "c2", planSetTool, `{"goal":"цел","steps":[{"text":"отвори","tool":"web_fetch"}]}`)},
	}
	for i := 0; i < 6; i++ {
		script = append(script, []openai.ChatCompletionStreamResponse{toolCallChunk(0, "f", "web_fetch", `{"url":"https://example.com/paper"}`)})
	}
	script = append(script, []openai.ChatCompletionStreamResponse{textChunk("Стига толкова.")})
	mock := &llm.MockLLM{StreamScript: script}
	o := planOrchestrator(mock, e, store)

	content, _, executed, err := o.generateResponse(ctx,
		[]openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "за всеки източник направи файл"}}, o.turnTools(), nil)

	require.NoError(t, err)
	assert.Equal(t, "Стига толкова.", content)
	assert.Equal(t, []string{planStubTool, planSetTool, "web_fetch", "web_fetch", "web_fetch", "web_fetch", "web_fetch"}, toolCallNames(executed),
		"plan_set and five calls: six rounds for tools, the sixth fetch is not run")
}
