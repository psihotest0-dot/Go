package agent

import "go-monolith/metrics"

// The results of a tool call, for metrics.ToolCalls.
const (
	toolResultOK      = "ok"
	toolResultRefused = "refused"
	toolResultFailed  = "failed"
	toolResultBadArgs = "bad_args"
)

// unknownToolLabel is the label of a tool name the server does not know.
const unknownToolLabel = "unknown"

// knownTools are the names of the tools the server can run, with the label each is counted
// under (an old name counts as the tool it is now). A name not here is "unknown": the name
// comes from the model, and a label that takes any text would grow without bound.
var knownTools = map[string]string{
	"create_memory":         "create_memory",
	"search_memories":       "search_memories",
	searchDocumentsTool:     searchDocumentsTool,
	"search_knowledge_base": searchDocumentsTool,
	listDocumentsTool:       listDocumentsTool,
	readAttachmentTool:      readAttachmentTool,
	addToLibraryTool:        addToLibraryTool,
	"save_global_rule":      "save_global_rule",
	styleNoteTool:           styleNoteTool,
	"web_search":            "web_search",
	"web_fetch":             "web_fetch",
	filesStubTool:           filesStubTool,
	listFilesTool:           listFilesTool,
	readFileTool:            readFileTool,
	writeFileTool:           writeFileTool,
	appendFileTool:          appendFileTool,
	planStubTool:            planStubTool,
	planSetTool:             planSetTool,
	planUpdateTool:          planUpdateTool,
}

// toolLabel is the label a call of name is counted under.
func toolLabel(name string) string {
	if label, ok := knownTools[name]; ok {
		return label
	}
	return unknownToolLabel
}

func countToolCall(name, result string) {
	metrics.ToolCalls.WithLabelValues(toolLabel(name), result).Inc()
}

func init() {
	// The series exist at zero from the start, so that increase() sees the first call of a
	// kind (as the Oracle series do; see metrics.init).
	for _, label := range knownTools {
		for _, result := range []string{toolResultOK, toolResultRefused, toolResultFailed, toolResultBadArgs, toolResultDuplicate} {
			metrics.ToolCalls.WithLabelValues(label, result)
		}
	}
}
