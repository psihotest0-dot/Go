package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/proposals"
	"go-monolith/internal/userfiles"
	"go-monolith/metrics"
)

// The tools for the user's own files (plan item P44). They are a toolGroup: only the
// stub, files, is on offer until the model calls it.
const (
	filesStubTool  = "files"
	listFilesTool  = "list_files"
	readFileTool   = "read_file"
	writeFileTool  = "write_file"
	appendFileTool = "append_file"
)

// filesStubTokens is what the stub costs in the request, measured with the tokenizer of
// the model (110 on 2026-11-14); it is reserved on top of fixedPromptTokens when files
// are enabled. The four tools cost 281 more, in the rounds after the model asked for them.
const filesStubTokens = 110

// filesGroup is the group of the file tools.
func filesGroup() toolGroup {
	return toolGroup{name: "files", stub: filesStubDefinition(), tools: fileToolDefinitions()}
}

// filesStubDefinition: the wording is the one measured (91 % of the requests that need
// files, 0 of 36 that do not): the examples are what makes it get the plain reads and
// listings. Change it only with a new measurement.
func filesStubDefinition() openai.Tool {
	return openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
		Name: filesStubTool,
		Description: "Save text to a file on the user's computer, open or read a file, or list files and folders. " +
			"Call it whenever the user wants something written down, saved, kept, exported or opened as a file or document, " +
			"even if they do not say 'file'. Examples: 'save this', 'put it in notes.txt', 'read data.csv', 'what is in the folder?'. " +
			"It gives you the file tools.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}}
}

func fileToolDefinitions() []openai.Tool {
	object := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	number := func(description string) map[string]any {
		return map[string]any{"type": "integer", "description": description}
	}
	tool := func(name, description string, parameters map[string]any) openai.Tool {
		return openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: name, Description: description, Parameters: parameters}}
	}
	return []openai.Tool{
		tool(listFilesTool, "List the files in the user's files folder.", object(map[string]any{})),
		tool(readFileTool, "Read a text file from the user's files folder, a part at a time. If next_start_line is not 0, call again with it to read on.",
			object(map[string]any{
				"name":       text("File name, e.g. notes.md."),
				"start_line": number("The first line to read (default 1)."),
				"max_lines":  number(fmt.Sprintf("How many lines to read (default %d).", userfiles.DefaultReadLines)),
			}, "name")),
		tool(writeFileTool, "Create a text file in the user's files folder. If the file exists, the user must confirm replacing it. Say that it is saved only after the tool answers \"saved\".",
			object(map[string]any{
				"name":    text("File name, e.g. notes.md."),
				"content": text("The full text of the file."),
			}, "name", "content")),
		tool(appendFileTool, "Add text to the end of a file in the user's files folder.",
			object(map[string]any{
				"name":    text("File name."),
				"content": text("The text to add."),
			}, "name", "content")),
	}
}

// WithFiles lets the file tools work on the folder of the user's files.
func (r *DefaultToolRegistry) WithFiles(store *userfiles.Store) *DefaultToolRegistry {
	r.files = store
	return r
}

// untrustedForFiles tells whether the turn has read text the user did not write: a web
// page, a book passage, an attached file, a file of the folder. A write in such a turn is
// not done at once; it is a proposal. (The passages that come with the prompt itself,
// sourceRetrievedDocuments, are not counted: they are there in most turns and would turn
// every write into a question.)
func untrustedForFiles(turn *turnState) bool {
	for _, source := range turn.externalSources() {
		if source != sourceRetrievedDocuments {
			return true
		}
	}
	return false
}

func fileOp(op, result string) { metrics.UserFileOps.WithLabelValues(op, result).Inc() }

// fileToolError turns a refusal of the file store into what the model sees: a call it can
// fix, or an expected refusal. Anything else is a failure of the server.
func (r *DefaultToolRegistry) fileToolError(tool string, err error) error {
	var invalid *userfiles.InvalidError
	limits := r.files.Limits()
	switch {
	case errors.As(err, &invalid):
		return &toolArgumentError{tool: tool, reason: invalid.Reason}
	case errors.Is(err, userfiles.ErrTooManyFiles):
		return &toolArgumentError{tool: tool, reason: fmt.Sprintf(
			"the user already has %d files; ask them to delete one before making another", limits.MaxFiles)}
	case errors.Is(err, userfiles.ErrQuotaExceeded):
		return &toolArgumentError{tool: tool, reason: "there is no room for more text in the user's files; ask them to delete something"}
	}
	return fmt.Errorf("failed to use the user's files: %w", err)
}

// fileNames lists up to 20 names for a message to the model.
func (r *DefaultToolRegistry) fileNames(userID uuid.UUID) string {
	list, err := r.files.List(userID)
	if err != nil || len(list) == 0 {
		return "none"
	}
	names := make([]string, 0, 20)
	for _, f := range list {
		if len(names) == 20 {
			break
		}
		names = append(names, f.Name)
	}
	return strings.Join(names, ", ")
}

func (r *DefaultToolRegistry) executeFilesUnlock() (interface{}, error) {
	return map[string]interface{}{
		"status": "ok",
		"message": "The file tools are now available: " + listFilesTool + ", " + readFileTool + ", " + writeFileTool + ", " + appendFileTool +
			". Use them now to do what the user asked.",
	}, nil
}

func (r *DefaultToolRegistry) executeListFiles(userID uuid.UUID) (interface{}, error) {
	list, err := r.files.List(userID)
	if err != nil {
		fileOp("list", "failed")
		return nil, r.fileToolError(listFilesTool, err)
	}
	fileOp("list", "ok")
	files := make([]map[string]interface{}, 0, len(list))
	for _, f := range list {
		files = append(files, map[string]interface{}{"name": f.Name, "size_bytes": f.Size, "modified": f.Modified.Format("2006-01-02 15:04")})
	}
	result := map[string]interface{}{"status": "success", "count": len(files), "files": files}
	if len(files) == 0 {
		result["message"] = "There are no files yet."
	}
	return result, nil
}

func (r *DefaultToolRegistry) executeReadFile(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	name, ok := stringArg(args, "name")
	if !ok {
		return nil, missingArgError(readFileTool, "name", `{"name": "notes.md"}`)
	}
	turn := turnFrom(ctx)
	if turn.has(sourceWebSearch) || turn.has(sourceWebFetch) {
		fileOp("read", "refused")
		return nil, &toolArgumentError{tool: readFileTool, reason: "files cannot be read in a message where the web was used; " +
			"tell the user to ask for the file in a separate message"}
	}

	part, err := r.files.Read(userID, name, intArg(args, "start_line", 1), intArg(args, "max_lines", userfiles.DefaultReadLines))
	switch {
	case err == nil:
	case errors.Is(err, userfiles.ErrNotFound):
		fileOp("read", "not_found")
		return map[string]interface{}{
			"status":  "not_found",
			"message": "No file with this name. Files: " + r.fileNames(userID),
		}, nil
	default:
		fileOp("read", "failed")
		return nil, r.fileToolError(readFileTool, err)
	}

	// The text of a file may have been saved from a page or a book: it is not the user's
	// own words, and what the turn writes after it goes through the user.
	turn.markExternal(sourceUserFiles)
	fileOp("read", "ok")
	result := map[string]interface{}{
		"status":          "success",
		"name":            part.Name,
		"total_lines":     part.TotalLines,
		"start_line":      part.StartLine,
		"end_line":        part.EndLine,
		"next_start_line": part.NextStartLine,
		"content":         part.Content,
	}
	if part.Content == "" {
		result["message"] = fmt.Sprintf("The file has only %d lines.", part.TotalLines)
	}
	return result, nil
}

func (r *DefaultToolRegistry) executeWriteFile(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	const example = `{"name": "notes.md", "content": "<the text>"}`
	name, ok := stringArg(args, "name")
	if !ok {
		return nil, missingArgError(writeFileTool, "name", example)
	}
	content, ok := stringArg(args, "content")
	if !ok {
		return nil, missingArgError(writeFileTool, "content", example)
	}
	turn := turnFrom(ctx)

	_, statErr := r.files.Stat(userID, name)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, userfiles.ErrNotFound) {
		fileOp("write", "failed")
		return nil, r.fileToolError(writeFileTool, statErr)
	}

	// Replacing a file, or writing in a turn that read text from outside, is not done at
	// once: the user confirms it.
	if exists || untrustedForFiles(turn) {
		mode := proposals.WriteCreate
		if exists {
			mode = proposals.WriteReplace
		}
		return r.proposeWrite(ctx, userID, writeFileTool, mode, name, content)
	}

	info, err := r.files.Create(userID, name, content)
	switch {
	case err == nil:
	case errors.Is(err, userfiles.ErrExists):
		// Made between the look and the write: the user decides.
		return r.proposeWrite(ctx, userID, writeFileTool, proposals.WriteReplace, name, content)
	default:
		fileOp("write", "refused")
		return nil, r.fileToolError(writeFileTool, err)
	}
	fileOp("write", "saved")
	slog.InfoContext(ctx, "a file was saved", "user_id", userID, "name", info.Name, "size_bytes", info.Size)
	return map[string]interface{}{
		"status":     "saved",
		"name":       info.Name,
		"size_bytes": info.Size,
		"message":    "The file is saved. Tell the user its name.",
	}, nil
}

func (r *DefaultToolRegistry) executeAppendFile(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	const example = `{"name": "notes.md", "content": "<the text to add>"}`
	name, ok := stringArg(args, "name")
	if !ok {
		return nil, missingArgError(appendFileTool, "name", example)
	}
	content, ok := stringArg(args, "content")
	if !ok {
		return nil, missingArgError(appendFileTool, "content", example)
	}

	if untrustedForFiles(turnFrom(ctx)) {
		return r.proposeWrite(ctx, userID, appendFileTool, proposals.WriteAppend, name, content)
	}

	info, err := r.files.Append(userID, name, content)
	switch {
	case err == nil:
	case errors.Is(err, userfiles.ErrNotFound):
		fileOp("append", "not_found")
		return map[string]interface{}{
			"status":  "not_found",
			"message": "No file with this name. Create it with " + writeFileTool + ". Files: " + r.fileNames(userID),
		}, nil
	default:
		fileOp("append", "refused")
		return nil, r.fileToolError(appendFileTool, err)
	}
	fileOp("append", "appended")
	slog.InfoContext(ctx, "text was added to a file", "user_id", userID, "name", info.Name, "size_bytes", info.Size)
	return map[string]interface{}{
		"status":     "appended",
		"name":       info.Name,
		"size_bytes": info.Size,
		"message":    "The text is added. Tell the user.",
	}, nil
}

// proposeWrite leaves a write for the user to confirm and tells the model so.
func (r *DefaultToolRegistry) proposeWrite(ctx context.Context, userID uuid.UUID, tool, mode, name, content string) (interface{}, error) {
	turn := turnFrom(ctx)
	p, err := r.proposals.ProposeWrite(ctx, userID, turn.session(), mode, name, content, turn.externalSources())
	switch {
	case err == nil:
	case errors.Is(err, userfiles.ErrNotFound):
		fileOp("propose", "not_found")
		return map[string]interface{}{
			"status":  "not_found",
			"message": "No file with this name. Create it with " + writeFileTool + ". Files: " + r.fileNames(userID),
		}, nil
	case errors.Is(err, userfiles.ErrExists):
		fileOp("propose", "exists")
		return nil, &toolArgumentError{tool: tool, reason: "a file with this name exists; the user must confirm replacing it. Call " + writeFileTool + " again"}
	default:
		fileOp("propose", "refused")
		return nil, proposalError(tool, err)
	}
	turn.addProposal(p)
	fileOp("propose", "created")
	result := pendingApprovalResult(p)
	switch mode {
	case proposals.WriteReplace:
		result["message"] = "NOT saved yet. A file with this name exists, and replacing it needs the user's confirmation " +
			"(the old text is kept as the previous version). Tell the user briefly what you propose and that they can confirm or reject it. " +
			"Do not say that it is saved."
	default:
		result["message"] = "NOT saved yet. This write waits for the user to confirm it, because this message used text from outside. " +
			"Tell the user briefly what you propose and that they can confirm or reject it. Do not say that it is saved."
	}
	return result, nil
}
