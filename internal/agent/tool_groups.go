package agent

import (
	"slices"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/metrics"
)

// A toolGroup is a set of tools that is not on offer from the start of a turn. Every tool
// on offer is described in the request, which costs context and time on every turn
// (about 0.46 ms and a token each), and a model with many tools picks worse. A group
// puts one small stub in its place; when the model calls the stub, the tools of the group
// are offered in the rest of the turn, in the place of the stub.
//
// What a group is for is told by the description of its stub, which the model reads
// every turn: it was measured (plan item P44, 2026-11-14) that a stub with examples gets
// the right call in 91 % of the requests that need the group and 0 of 36 otherwise. A tool
// that the model uses on its own, without being asked, is not a candidate: the rules and the
// memory tools went from 6 of 6 to 3 of 6 when they were hidden behind a stub.
type toolGroup struct {
	name string
	// stub is the tool offered in the place of the group while it is locked.
	stub openai.Tool
	// tools replace the stub once the model has called it.
	tools []openai.Tool
}

func (g toolGroup) stubName() string { return g.stub.Function.Name }

// turnTools are the tools offered when a turn starts: the ones that are always on offer and
// the stub of every group.
func (o *Orchestrator) turnTools() []openai.Tool {
	tools := toolDefinitions()
	for _, g := range o.groups {
		tools = append(tools, g.stub)
	}
	return tools
}

// onlyUnlocks reports whether every call of a round is the stub of a group that is still
// locked. Such a round only changes what is on offer; it does not count against the
// limit of tool rounds, which is for tools that do something. A group unlocks once (its
// stub is gone afterwards), so this cannot go on for ever.
func (o *Orchestrator) onlyUnlocks(active []openai.Tool, calls []openai.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		if !o.isLockedStub(active, call.Function.Name) {
			return false
		}
	}
	return true
}

func (o *Orchestrator) isLockedStub(active []openai.Tool, name string) bool {
	for _, g := range o.groups {
		if g.stubName() == name && hasTool(active, name) {
			return true
		}
	}
	return false
}

// nextTools returns the tools to offer in the next round of the turn, after the round that
// made calls:
//
//   - a group whose stub was called is unlocked;
//   - the user's files and the web are not used in the same turn: the model may have read
//     a page or a book that tells it to read a file and send the text out (plan item P44,
//     the "lethal trifecta": private data, text from outside, a way out). So a turn that
//     read a file offers no web tools, and a turn that used the web offers no read_file.
//     The tools check the same thing themselves (see the registry); this keeps the model
//     from being offered what would be refused.
func (o *Orchestrator) nextTools(active []openai.Tool, calls []openai.ToolCall, turn *turnState) []openai.Tool {
	next := slices.Clone(active)
	for _, g := range o.groups {
		if !hasTool(next, g.stubName()) || !slices.ContainsFunc(calls, func(c openai.ToolCall) bool { return c.Function.Name == g.stubName() }) {
			continue
		}
		next = replaceTool(next, g.stubName(), g.tools)
		metrics.ToolGroupUnlocks.WithLabelValues(g.name).Inc()
	}
	if turn.has(sourceUserFiles) {
		next = withoutTools(next, "web_search", "web_fetch")
	}
	if turn.has(sourceWebSearch) || turn.has(sourceWebFetch) {
		next = withoutTools(next, readFileTool)
	}
	return next
}

func hasTool(tools []openai.Tool, name string) bool {
	return slices.ContainsFunc(tools, func(t openai.Tool) bool { return t.Function != nil && t.Function.Name == name })
}

// replaceTool puts with in the place of the tool called name.
func replaceTool(tools []openai.Tool, name string, with []openai.Tool) []openai.Tool {
	out := make([]openai.Tool, 0, len(tools)+len(with))
	for _, t := range tools {
		if t.Function != nil && t.Function.Name == name {
			out = append(out, with...)
			continue
		}
		out = append(out, t)
	}
	return out
}

func withoutTools(tools []openai.Tool, names ...string) []openai.Tool {
	out := make([]openai.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Function != nil && slices.Contains(names, t.Function.Name) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// addedTools are the tools in after that were not in before.
func addedTools(before, after []openai.Tool) []openai.Tool {
	var added []openai.Tool
	for _, t := range after {
		if t.Function != nil && !hasTool(before, t.Function.Name) {
			added = append(added, t)
		}
	}
	return added
}
