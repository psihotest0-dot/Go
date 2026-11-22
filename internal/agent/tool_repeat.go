package agent

import (
	"encoding/json"
	"fmt"
)

// A call of a tool that repeats one the turn has already made - the same tool, the same
// arguments - is not run again (plan item 0.3, the idea I3 from the review of AnythingLLM,
// 2026-11-22). The model gets a short answer that it has done it in this turn and the result is
// above, and the call is counted as "duplicate". A small model that does not trust a result,
// or lost its place, calls the same search or fetch two and three times; each time the
// result takes the room of the context again, and a write or a memory is made twice.
//
// What is the same: the tool under the name it is counted by (an old name is the tool it is
// now) and the arguments as canonical JSON (encoding/json writes the keys of a map in order,
// so {"b":1,"a":2} and {"a":2,"b":1} are one call). Only a call that worked is remembered: one
// the server refused or failed may be tried again.
//
// A successful call of a tool that changes something (changingTools) forgets the calls that
// came before it, and keeps only itself: "read the file, write it, read it again" reads a
// different file the second time. The memory of the calls lasts for one run of the rounds
// (runRounds): a retry of the turn (files_guard.go) starts from the messages without the results
// of the first try, so "the result is above" would not be true there.

// toolResultDuplicate is the result of a call that repeated one of the turn (metrics.ToolCalls).
const toolResultDuplicate = "duplicate"

// changingTools are the tools whose call changes what a later call of another tool returns.
var changingTools = map[string]bool{
	"create_memory": true, "save_global_rule": true, styleNoteTool: true, addToLibraryTool: true,
	writeFileTool: true, appendFileTool: true, planSetTool: true, planUpdateTool: true,
}

// callKey is what makes two calls the same call.
func callKey(name string, args map[string]interface{}) string {
	tool := name
	if label, ok := knownTools[name]; ok {
		tool = label
	}
	canonical, err := json.Marshal(args)
	if err != nil {
		// Arguments that came from JSON go back to JSON; if not, the call is never a repeat.
		return ""
	}
	return tool + "\x00" + string(canonical)
}

// repeatedCallResult is what the model gets for a call that repeated one of the turn.
func repeatedCallResult(tool string) string {
	return fmt.Sprintf("Not run again: you already called %s with exactly these arguments in this turn, "+
		"and its result is above. Use that result; call it again only with different arguments.", tool)
}

// toolLogRepeat is the line of the turn's record for such a call.
func toolLogRepeat(tool string) string {
	if silentTools[tool] {
		return ""
	}
	return tool + ": the same call again - not run, the earlier result was used"
}

// ranBefore reports whether the turn has already made this call with success.
func (t *turnState) ranBefore(key string) bool {
	if t == nil || key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.calls[key]
	return ok
}

// rememberCall records a call that worked. A tool that changes something forgets the calls
// before it (see the top of this file).
func (t *turnState) rememberCall(tool, key string) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.calls == nil || changingTools[toolLabel(tool)] {
		t.calls = map[string]struct{}{}
	}
	t.calls[key] = struct{}{}
}

// forgetCalls starts the memory of the calls again, for a new run of the rounds.
func (t *turnState) forgetCalls() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = nil
}
