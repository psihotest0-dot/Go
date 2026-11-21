package agent

import (
	"fmt"
	"strings"
	"unicode"
)

// The record of what the tools did in a turn (messages.tool_log, migration 00035).
//
// The history of a conversation kept the words of the answers and not what their tools did, so
// the model read "Създадох файла" or an "extraction" of a paper in it and could not tell an answer
// that did the work from one that only said so: a "продължи" or "успя ли?" was answered from its
// own earlier words (the chats of 2026-11-14, 16, 20 and 21). The server now writes one short line
// per call of the turn from the results of the tools - never from the model's words - stores them
// with the answer and shows them to the model next to that answer in the history (toolLogNote).
// The person does not see them.
//
// A line holds the tool, what it was called on (a name, a query, the host and path of a link) and
// how it ended. It never holds text the tool read (a page, a passage, a file), so the record adds
// no untrusted text to the history.

const (
	// maxToolLogLines is how many calls of a turn the record keeps; the rest are counted.
	maxToolLogLines = 10
	// maxToolLogArgRunes cuts what a call was made on (a query, a name, a link).
	maxToolLogArgRunes = 60
	// maxToolLogReasonRunes cuts the reason a call was refused.
	maxToolLogReasonRunes = 90
	// toolLogSeparator joins the lines of a record when it is stored.
	toolLogSeparator = "\n"
)

// silentTools leave no line: their call does nothing for the person (a stub that only unlocks a
// group) or is shown elsewhere (the plan is in its own note).
var silentTools = map[string]bool{filesStubTool: true, planStubTool: true, planSetTool: true, planUpdateTool: true}

// toolLogArg is what a call was made on, on one line and cut short: the first of the arguments
// that is there.
func toolLogArg(args map[string]interface{}, names ...string) string {
	for _, n := range names {
		s, ok := args[n].(string)
		if !ok {
			continue
		}
		s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '[' || r == ']' }), " ")
		if s != "" {
			return truncateRunes(s, maxToolLogArgRunes)
		}
	}
	return ""
}

// linkArg is a link without its scheme, cut short: enough to tell which page it was.
func linkArg(args map[string]interface{}) string {
	link := toolLogArg(args, "url")
	for _, scheme := range []string{"https://", "http://"} {
		link = strings.TrimPrefix(link, scheme)
	}
	return strings.TrimPrefix(link, "www.")
}

func resultStatus(res interface{}) string {
	m, ok := res.(map[string]interface{})
	if !ok {
		return ""
	}
	status, _ := m["status"].(string)
	return status
}

// resultCount is the length of the list a result holds under key (0 when there is none).
func resultCount(res interface{}, key string) int {
	m, ok := res.(map[string]interface{})
	if !ok {
		return 0
	}
	switch v := m[key].(type) {
	case []map[string]interface{}:
		return len(v)
	case []interface{}:
		return len(v)
	case []addRow:
		return len(v)
	case int:
		return v
	}
	return 0
}

// toolLogLine is the line of a successful call: what it did, from its result. size is the length
// of the result the model got. "" for a call that leaves no line.
func toolLogLine(tool string, args map[string]interface{}, res interface{}, size int) string {
	if silentTools[tool] {
		return ""
	}
	status := resultStatus(res)
	named := func(what string) string {
		if name := toolLogArg(args, "name"); name != "" {
			return fmt.Sprintf("%s %q: %s", tool, name, what)
		}
		return tool + ": " + what
	}
	switch tool {
	case "web_search":
		return fmt.Sprintf("web_search %q: results read (%.1f KB)", toolLogArg(args, "query"), float64(size)/1024)
	case "web_fetch":
		line := fmt.Sprintf("web_fetch %s: page read (%.1f KB)", linkArg(args), float64(size)/1024)
		if size < shortPageBytes {
			line += ", short - maybe only a summary"
		}
		return line
	case searchDocumentsTool, "search_knowledge_base":
		if status == "skipped" {
			return fmt.Sprintf("search_documents %q: not searched", toolLogArg(args, "query", "text"))
		}
		return fmt.Sprintf("search_documents %q: %d passages", toolLogArg(args, "query", "text"), resultCount(res, "results"))
	case "search_memories":
		return fmt.Sprintf("search_memories %q: %d memories", toolLogArg(args, "query"), resultCount(res, "results"))
	case listDocumentsTool:
		return fmt.Sprintf("list_documents: %d files", resultCount(res, "count"))
	case listFilesTool:
		return fmt.Sprintf("list_files: %d files", resultCount(res, "count"))
	case readAttachmentTool, readFileTool:
		if status == "not_found" {
			return named("not found")
		}
		return named("read")
	case writeFileTool, appendFileTool:
		switch status {
		case "saved":
			return named("saved")
		case "appended":
			return named("text added")
		case "pending_approval":
			return named("NOT saved - waits for the user's confirmation")
		case "not_found":
			return named("not found, nothing written")
		}
		return named("nothing written")
	case addToLibraryTool:
		if m, ok := res.(map[string]interface{}); ok {
			if summary, _ := m["summary"].(string); summary != "" {
				return "add_to_library: " + summary
			}
		}
		return "add_to_library: done"
	case "create_memory":
		return "create_memory: saved"
	case "save_global_rule", styleNoteTool:
		if status == "pending_approval" {
			return tool + ": proposed - waits for the user's confirmation"
		}
		return tool + ": done"
	}
	return tool + ": done"
}

// toolLogFailure is the line of a call that did not work. refused: the call was wrong (the
// reason is the server's own text, cut short); otherwise the server failed.
func toolLogFailure(tool, reason string, refused bool) string {
	if _, known := knownTools[tool]; !known {
		return fmt.Sprintf("%s: no such tool", truncateRunes(strings.Join(strings.Fields(tool), " "), 30))
	}
	if silentTools[tool] {
		return ""
	}
	if refused {
		return fmt.Sprintf("%s: refused (%s)", tool, truncateRunes(strings.Join(strings.Fields(reason), " "), maxToolLogReasonRunes))
	}
	return tool + ": failed (server error), nothing done"
}

// addToolLog records a line of the turn's record; an empty line is not one.
func (t *turnState) addToolLog(line string) {
	if t == nil || line == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toolLog = append(t.toolLog, line)
}

// storedToolLog is the record of the turn as it is stored with the answer: "" when no tool did
// anything, which is itself a fact the history shows (see toolLogNote).
func (t *turnState) storedToolLog() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := t.toolLog
	if len(lines) > maxToolLogLines {
		lines = append(lines[:maxToolLogLines:maxToolLogLines], fmt.Sprintf("and %d more calls", len(t.toolLog)-maxToolLogLines))
	}
	return strings.Join(lines, toolLogSeparator)
}

// toolLogNotePrefix starts the note; an answer of the model that copies it is cut there
// (sanitizeAssistantText), so the model cannot write a record of its own.
const toolLogNotePrefix = "[System record of the tools of this answer"

// noteAnswersWithoutTools: an answer whose turn called no tool also gets a note that says so.
// Off: measured against the note under answers with tools only (see the plan).
var noteAnswersWithoutTools = false

// historyToolNote is what the model reads after one of its answers in the history: what the tools
// of that turn did. log is the stored record ("" when the turn called no tool). An answer stored
// before the record existed has none (NULL) and gets no note: what its tools did is not known.
func historyToolNote(log string) string {
	log = strings.TrimSpace(log)
	if log == "" {
		if noteAnswersWithoutTools {
			return "\n\n" + toolLogNotePrefix + ", not written by you and not seen by the user: no tool was called, nothing was read or saved.]"
		}
		return ""
	}
	return "\n\n" + toolLogNotePrefix + ", not written by you and not seen by the user: " + strings.Join(strings.Split(log, toolLogSeparator), "; ") + ".]"
}
