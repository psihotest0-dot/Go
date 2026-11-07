package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/attachments"
	"go-monolith/metrics"
)

// readAttachmentTool names the tool that reads a file the user attached to the
// conversation. The prompt shows the files that fit a small budget; this reads any
// of them, a part at a time.
const readAttachmentTool = "read_attachment"

func attachmentToolDefinition() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name: readAttachmentTool,
			Description: "Look into a file the user attached to this conversation. " +
				"Use it for an attached file that the system prompt does not show in full. " +
				"To locate a function, a word or a piece of code in a big file, pass find: the result lists the lines that contain it, " +
				"with their numbers; then read around them with start_line. " +
				"Without find it returns lines with their numbers; if next_start_line is not 0, call again with it to read on.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "The name of the attached file, as the system prompt lists it.",
					},
					"find": map[string]interface{}{
						"type":        "string",
						"description": "Optional. Text to look for in the file (any letter case). Returns the matching lines and their numbers instead of a reading.",
					},
					"start_line": map[string]interface{}{
						"type":        "integer",
						"description": "The first line to read (default 1).",
					},
					"max_lines": map[string]interface{}{
						"type":        "integer",
						"description": fmt.Sprintf("How many lines to read (default %d, at most %d).", attachments.DefaultReadLines, attachments.MaxReadLines),
					},
				},
				"required": []string{"name"},
			},
		},
	}
}

func (r *DefaultToolRegistry) executeReadAttachment(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	name, ok := stringArg(args, "name")
	if !ok {
		return nil, missingArgError(readAttachmentTool, "name", `{"name": "main.go"}`)
	}
	sessionID := turnFrom(ctx).session()
	if r.attachments == nil || sessionID == "" {
		return nil, errors.New("no files can be attached to this conversation")
	}

	file, err := r.attachments.Find(ctx, userID, sessionID, name)
	switch {
	case err == nil:
	case errors.Is(err, attachments.ErrNotFound), errors.Is(err, attachments.ErrAmbiguous):
		// The server is fine and the model can fix the call: tell it which files there are.
		return map[string]interface{}{
			"status":  "not_found",
			"message": err.Error() + ". Attached files: " + r.attachedNames(ctx, userID, sessionID),
		}, nil
	default:
		return nil, fmt.Errorf("failed to read the attached file: %w", err)
	}

	// The text of a file is not the user's own words: it may be a web page or a log
	// with instructions in it. What the turn writes from it goes through the user.
	turnFrom(ctx).markExternal(sourceAttachments)
	metrics.ChatAttachments.WithLabelValues("read").Inc()

	if text, ok := stringArg(args, "find"); ok {
		found := attachments.FindText(file, text)
		result := map[string]interface{}{
			"status":        "success",
			"name":          found.Name,
			"total_lines":   found.TotalLines,
			"total_matches": found.TotalMatches,
			"matches":       found.Matches,
		}
		switch {
		case found.TotalMatches == 0:
			result["message"] = "No line contains that text. Try another word, or read the file in parts."
		case found.TotalMatches > len(found.Matches):
			result["message"] = fmt.Sprintf("Only the first %d of %d matching lines are shown; search for something more specific.", len(found.Matches), found.TotalMatches)
		}
		return result, nil
	}

	part := attachments.Read(file, intArg(args, "start_line", 1), intArg(args, "max_lines", attachments.DefaultReadLines))
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

func (r *DefaultToolRegistry) attachedNames(ctx context.Context, userID uuid.UUID, sessionID string) string {
	list, err := r.attachments.List(ctx, userID, sessionID)
	if err != nil || len(list) == 0 {
		return "none"
	}
	names := make([]string, len(list))
	for i, f := range list {
		names[i] = f.Name
	}
	return strings.Join(names, ", ")
}
