package agent

import (
	"fmt"

	openai "github.com/sashabaranov/go-openai"
)

// The buttons of the modes (the owner's idea, 2026-11-23/24): a mode chosen in the client sends
// the next message down one road whatever it says - for the times the person does not remember
// how a tool is asked for. Without a mode everything is as before. The mode is for one message
// (the client clears it after the send); the server keeps nothing of it.
//
// A mode offers the turn its tools and requires the first round to call one of them, as the
// retry of a turn that claimed a write does (files_guard.go): with "required" and only those
// tools on offer, llama-server has the model call one (a tool_choice that names a function it
// ignores). What the model writes into the call - the query, the text of a file - stays its own.
// The answers of the code (the help list, "успя ли?", the task over the added documents) are not
// given in a mode: the person asked for the road.
//
// Why: of the 11 corrections under the answers of the owner's real conversations in 14 days
// (2026-11-10..24), 7 were "asked for a write into a file, no file tool was called" and 4 "the
// work promised for later, nothing done".
const (
	ModeBooks   = "books"   // the books of the library: search_documents, list_documents
	ModeWeb     = "web"     // the internet: web_search, web_fetch
	ModeLibrary = "library" // a link into the library: add_to_library
	ModeFiles   = "files"   // Lili's own files: the group of the files
	ModeMemory  = "memory"  // the memory: create_memory, search_memories
	ModeStyle   = "style"   // how to talk: save_global_rule, save_style_note
)

// modeLabels are the names of the modes as the buttons have them, for the answers of the code.
var modeLabels = map[string]string{
	ModeBooks:    "Книги",
	ModeWeb:      "Интернет",
	ModeLibrary:  "Библиотека",
	ModeFiles:    "Бележките на Лили",
	ModeMemory:   "Памет",
	ModeStyle:    "Как да говорим",
	ModeReminder: "Напомняне",
	ModeAnalyze:  "Анализ",
}

// validMode tells whether a request may carry the mode: "" (none) or one of the modes.
func validMode(mode string) bool {
	_, ok := modeLabels[mode]
	return mode == "" || ok
}

// modeTools are the tools of a turn in a mode: open is what the turn offers, required what its
// first round must call. required is empty when the turn does not offer the mode's tools (the
// feature is off, or - for the library - the conversation has no link the person wrote).
func (o *Orchestrator) modeTools(tools []openai.Tool, mode string, turn *turnState) (open, required []openai.Tool) {
	pick := func(names ...string) []openai.Tool {
		var out []openai.Tool
		for _, name := range names {
			out = append(out, onlyTool(tools, name)...)
		}
		return out
	}
	switch mode {
	case ModeBooks:
		return tools, pick(searchDocumentsTool, listDocumentsTool)
	case ModeWeb:
		return tools, pick("web_search", "web_fetch")
	case ModeLibrary:
		return tools, pick(addToLibraryTool)
	case ModeFiles:
		if hasTool(tools, filesStubTool) {
			return o.retryFileTools(tools, turn)
		}
		// Already open (a test may give the tools themselves).
		return tools, pick(listFilesTool, readFileTool, writeFileTool, appendFileTool, savePageTool)
	case ModeMemory:
		return tools, pick("create_memory", "search_memories")
	case ModeStyle:
		return tools, pick("save_global_rule", styleNoteTool)
	}
	return tools, nil
}

// modeOffText is the answer of the code when the turn has no tools for its mode.
func modeOffText(mode string) string {
	if mode == ModeLibrary {
		return "За режим „Библиотека“ ми трябва линк: напиши го в съобщението (може и няколко), например „Добави: https://…“."
	}
	return fmt.Sprintf("Режимът „%s“ сега не е включен на сървъра, затова не мога да мина по него. Напиши ми съобщението без режим и ще отговоря както обикновено.", modeLabels[mode])
}

func (t *turnState) setMode(mode string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mode = mode
}

// modeName is the mode of the turn, "" for none.
func (t *turnState) modeName() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mode
}

// knownModes lists the modes in the order of the buttons (for the help text and the tests).
func knownModes() []string {
	return []string{ModeBooks, ModeWeb, ModeLibrary, ModeFiles, ModeMemory, ModeStyle, ModeReminder, ModeAnalyze}
}
