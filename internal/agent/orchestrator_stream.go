package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

func (o *Orchestrator) streamResponse(w http.ResponseWriter, r *http.Request, finalMessages []openai.ChatCompletionMessage, tools []openai.Tool, sources []api.SourceInfo) (string, []openai.ToolCall, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("streaming unsupported", "err", fmt.Errorf("response writer does not support flushing"))
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return "", nil, errors.New("streaming unsupported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sourcesBytes, _ := json.Marshal(sources)
	fmt.Fprintf(w, "event: sources\ndata: %s\n\n", string(sourcesBytes))
	flusher.Flush()

	onDelta := func(content, reasoning string) error {
		if reasoning != "" {
			dataMap := map[string]string{"reasoning_content": reasoning}
			dataBytes, _ := json.Marshal(dataMap)
			fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
			flusher.Flush()
		}

		if content != "" {
			dataMap := map[string]string{"content": content}
			dataBytes, _ := json.Marshal(dataMap)
			fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
			flusher.Flush()
		}
		return nil
	}

	assistantContent, _, toolCalls, err := o.generateResponse(r.Context(), finalMessages, tools, sources, onDelta)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return assistantContent, toolCalls, err
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	return assistantContent, toolCalls, nil
}
