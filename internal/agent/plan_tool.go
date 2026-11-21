package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/taskplan"
	"go-monolith/metrics"
)

// The plan of a task (package taskplan): the tools with which the model makes and keeps it,
// and what the server does around them. They are a toolGroup: only the stub, plan, is on offer
// until the model calls it - except in a conversation that has a plan in progress, where the
// tools are on offer from the start of the turn and the plan is in the note next to the message
// of the person (see withPlanNote).
const (
	planStubTool   = "plan"
	planSetTool    = "plan_set"
	planUpdateTool = "plan_update"

	// planActiveRounds are the rounds for tools a turn gets more when the conversation has a
	// plan in progress: a step is a call, and two rounds were what stopped the work of an hour
	// of the chat of 2026-11-21 ("tool round limit reached").
	planActiveRounds = 4

	// planNoteMaxRunes caps the note, whatever the plan holds.
	planNoteMaxRunes = 2400
)

// planGroup is the group of the plan tools.
func planGroup() toolGroup {
	return toolGroup{name: "plan", stub: planStubDefinition(), tools: planToolDefinitions()}
}

func planStubDefinition() openai.Tool {
	return openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
		Name: planStubTool,
		Description: "Make a step-by-step plan for a task that has several steps or many items, and keep it. " +
			"Call it BEFORE you start such a task. Examples: 'get these 21 sources', 'read these papers and compare them', " +
			"'collect everything about X into files'. It gives you plan_set and plan_update.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}}
}

func planToolDefinitions() []openai.Tool {
	step := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "What to do in this step, one action."},
			"tool": map[string]any{"type": "string", "enum": taskplan.StepTools, "description": "The tool that does it; \"none\" for a step that is only a decision or the answer to the user."},
		},
		"required": []string{"text"},
	}
	return []openai.Tool{
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: planSetTool,
			Description: fmt.Sprintf("Make the plan of the task: a short goal and 1 to %d steps. Name the tool of a step when one tool does it: "+
				"the step is then done only after that tool succeeded. Replaces the plan in progress. Then do the first step now.", taskplan.MaxSteps),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal":  map[string]any{"type": "string", "description": "What the task is, in one sentence."},
					"steps": map[string]any{"type": "array", "items": step},
				},
				"required": []string{"goal", "steps"},
			},
		}},
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: planUpdateTool,
			Description: "Report on a step of the plan: doing, done, failed or skipped. 'done' is accepted only after a successful call of the step's tool; " +
				"'failed' and 'skipped' need a note with the reason. Then do the next step.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"step":   map[string]any{"type": "integer", "description": "The number of the step."},
					"status": map[string]any{"type": "string", "enum": []string{"doing", "done", "failed", "skipped"}},
					"note":   map[string]any{"type": "string", "description": "The reason, for failed and skipped."},
				},
				"required": []string{"step", "status"},
			},
		}},
	}
}

// toolTokens estimates what the definitions of tools cost in a request.
func toolTokens(tools ...openai.Tool) int {
	raw, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return estimateTokens(string(raw))
}

// planStubTokens is what the stub costs in every request when the plan is on.
func planStubTokens() int { return toolTokens(planStubDefinition()) }

// WithPlans lets the plan tools work on the plans of the conversations.
func (r *DefaultToolRegistry) WithPlans(store *taskplan.Store) *DefaultToolRegistry {
	r.plans = store
	return r
}

// WithTaskPlans offers the plan of a task (package taskplan): the stub of the tools is on offer
// in every turn, the tools themselves after the model called it, or from the start in a
// conversation that has a plan in progress. The registry must have the store
// (DefaultToolRegistry.WithPlans).
func (o *Orchestrator) WithTaskPlans(store *taskplan.Store) *Orchestrator {
	o.plans = store
	o.groups = append(o.groups, planGroup())
	return o
}

func planEvent(event string) { metrics.TaskPlanEvents.WithLabelValues(event).Inc() }

// planError turns what the plan refused into what the model sees.
func planError(tool string, err error) error {
	var invalid *taskplan.InvalidError
	if errors.As(err, &invalid) {
		return &toolArgumentError{tool: tool, reason: invalid.Reason}
	}
	return fmt.Errorf("failed to use the plan: %w", err)
}

func (r *DefaultToolRegistry) executePlanStub() (interface{}, error) {
	return map[string]interface{}{
		"status": "ok",
		"message": "The plan tools are now available: " + planSetTool + ", " + planUpdateTool + ". Nothing is done for the user yet: " +
			"call " + planSetTool + " now with the goal and the steps of the task (name the tool of a step when one tool does it).",
	}, nil
}

func (r *DefaultToolRegistry) executePlanSet(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	const example = `{"goal": "<the task>", "steps": [{"text": "<step>", "tool": "add_to_library"}]}`
	goal, ok := stringArg(args, "goal")
	if !ok {
		return nil, missingArgError(planSetTool, "goal", example)
	}
	raw, ok := args["steps"].([]interface{})
	if !ok || len(raw) == 0 {
		return nil, missingArgError(planSetTool, "steps", example)
	}
	inputs := make([]taskplan.StepInput, 0, len(raw))
	for _, item := range raw {
		switch v := item.(type) {
		case string: // a model that sends the texts only is understood
			inputs = append(inputs, taskplan.StepInput{Text: v})
		case map[string]interface{}:
			text, _ := v["text"].(string)
			tool, _ := v["tool"].(string)
			inputs = append(inputs, taskplan.StepInput{Text: text, Tool: tool})
		default:
			return nil, &toolArgumentError{tool: planSetTool, reason: "every step is an object like " + `{"text": "<step>", "tool": "<tool>"}`}
		}
	}
	turn := turnFrom(ctx)
	p, err := r.plans.Set(ctx, userID, turn.session(), goal, inputs)
	if err != nil {
		return nil, planError(planSetTool, err)
	}
	turn.markPlanActive()
	planEvent("set")
	slog.InfoContext(ctx, "a task plan was made", "user_id", userID, "steps", len(p.Steps))
	first := p.Next()
	result := map[string]interface{}{
		"status": "ok",
		"steps":  len(p.Steps),
		"message": fmt.Sprintf("The plan is made. Do step %d now%s, then call %s for it. The plan is kept by the system and is in the note next to the user's messages.",
			first.ID, withTool(first.Tool), planUpdateTool),
	}
	if len(p.Warnings) > 0 {
		result["warnings"] = p.Warnings
	}
	return result, nil
}

func withTool(tool string) string {
	if tool == "" || tool == taskplan.ToolNone {
		return ""
	}
	return " with " + tool
}

func (r *DefaultToolRegistry) executePlanUpdate(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	const example = `{"step": 1, "status": "done"}`
	step := intArg(args, "step", 0)
	if step == 0 {
		return nil, missingArgError(planUpdateTool, "step", example)
	}
	status, ok := stringArg(args, "status")
	if !ok {
		return nil, missingArgError(planUpdateTool, "status", example)
	}
	note, _ := args["note"].(string)
	out, _, err := r.plans.Update(ctx, userID, turnFrom(ctx).session(), step, strings.ToLower(status), note)
	if err != nil {
		var invalid *taskplan.InvalidError
		if errors.As(err, &invalid) && strings.HasPrefix(invalid.Reason, "NOT done") {
			planEvent("step_refused")
		}
		return nil, planError(planUpdateTool, err)
	}
	planEvent("step_" + strings.ToLower(status))
	if out.Finished {
		planEvent("completed")
	}
	return map[string]interface{}{"status": "ok", "finished": out.Finished, "message": out.Message}, nil
}

// withPlanNote puts the plan of the conversation, when one is in progress, next to the last
// message of the person, in the copy the model reads (never stored, like the closing note).
// It returns the plan, or nil, and the tokens the plan tools cost more than their stub. A failure is only logged: the turn goes on without the plan.
func (o *Orchestrator) withPlanNote(ctx context.Context, userID uuid.UUID, sessionID string, c chatContext) (chatContext, *taskplan.Plan, int) {
	if o.plans == nil {
		return c, nil, 0
	}
	plan, err := o.plans.Active(ctx, userID, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "could not read the plan of the conversation; the turn goes on without it", "err", err, "session_id", sessionID)
		return c, nil, 0
	}
	if plan == nil {
		return c, nil, 0
	}
	note := taskplan.Render(plan)
	if len([]rune(note)) > planNoteMaxRunes {
		note = string([]rune(note)[:planNoteMaxRunes-1]) + "…]"
	}
	if last := len(c.History) - 1; last >= 0 && c.History[last].Role == openai.ChatMessageRoleUser {
		c.History[last].Content += note
	}
	// The note is part of the last message and is counted with the history; the tools of the plan
	// take the place of the stub, which the budget of the prompt has paid for.
	return c, plan, toolTokens(planToolDefinitions()...) - planStubTokens()
}

// evidenceTools are the tools whose success is evidence for a step of a plan.
var evidenceTools = map[string]bool{
	"web_search": true, "web_fetch": true, addToLibraryTool: true, searchDocumentsTool: true, listDocumentsTool: true,
	readAttachmentTool: true, readFileTool: true, writeFileTool: true, appendFileTool: true,
}

// shortPageBytes: a page this short is a summary and not the page.
const shortPageBytes = 3000

// planEvidence says what a successful call of a tool is worth to a plan: whether it is evidence
// at all (a search that found nothing, a file that was not there, a link that was refused are
// not), and the line that says what it did. size is the length of the result the model got.
func planEvidence(tool string, args map[string]interface{}, res interface{}, size int) (string, bool) {
	if !evidenceTools[tool] {
		return "", false
	}
	arg := func(names ...string) string {
		for _, n := range names {
			if s, ok := args[n].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	kb := func() string { return fmt.Sprintf("%.1f KB", float64(size)/1024) }
	status := ""
	if m, ok := res.(map[string]interface{}); ok {
		status, _ = m["status"].(string)
	}
	switch tool {
	case "web_search":
		size = webResultBytes(res, size)
		return fmt.Sprintf("web_search %q -> %s", arg("query"), kb()), size >= minWebResultBytes
	case "web_fetch":
		size = webResultBytes(res, size)
		gist := fmt.Sprintf("web_fetch %s -> %s", shortURL(arg("url")), kb())
		if size < shortPageBytes {
			gist += " (short: a summary?)"
		}
		return gist, size >= minWebResultBytes
	case addToLibraryTool:
		m, ok := res.(map[string]interface{})
		if !ok {
			return "", false
		}
		rows, _ := m["results"].([]addRow)
		added := 0
		for _, row := range rows {
			if row.Status == addQueued || row.Status == addDuplicate {
				added++
			}
		}
		summary, _ := m["summary"].(string)
		return "add_to_library: " + summary, added > 0
	case listDocumentsTool:
		count := 0
		if m, ok := res.(map[string]interface{}); ok {
			count, _ = m["count"].(int)
		}
		return fmt.Sprintf("list_documents -> %d files", count), count > 0
	case searchDocumentsTool:
		return fmt.Sprintf("search_documents %q -> %s", arg("query", "text"), kb()), size >= 100
	case readAttachmentTool, readFileTool:
		return fmt.Sprintf("%s %s -> %s", tool, arg("name"), kb()), status != statusNotFound
	case writeFileTool, appendFileTool:
		switch status {
		case statusSaved, statusAppended:
			return fmt.Sprintf("%s %s: %s", tool, arg("name"), status), true
		case statusPendingApproval:
			return fmt.Sprintf("%s %s: waiting for the user's confirmation", tool, arg("name")), true
		}
		return "", false
	}
	return "", false
}

// recordPlanEvidence writes what a tool just did in the ledger of the plan in progress, so that
// a step can be marked done on it. A conversation without a plan costs one query.
func (o *Orchestrator) recordPlanEvidence(ctx context.Context, tool string, args map[string]interface{}, res interface{}, size int) {
	if o.plans == nil {
		return
	}
	gist, ok := planEvidence(tool, args, res, size)
	if !ok {
		return
	}
	userID, hasUser := ctx.Value(api.ParsedUserIDKey).(uuid.UUID)
	turn := turnFrom(ctx)
	if !hasUser || turn == nil {
		return
	}
	plan, err := o.plans.Record(ctx, userID, turn.session(), tool, gist)
	if err != nil {
		slog.WarnContext(ctx, "could not record what a tool did in the plan", "tool", tool, "err", err)
		return
	}
	if plan != nil {
		planEvent("evidence")
	}
}
