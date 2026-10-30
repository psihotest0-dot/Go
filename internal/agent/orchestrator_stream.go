package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
)

type streamFilter struct {
	inThink bool
	buffer  string
}

func (f *streamFilter) Process(chunk string) string {
	f.buffer += chunk
	var out strings.Builder

	for len(f.buffer) > 0 {
		if f.inThink {
			idx := strings.Index(f.buffer, "</think>")
			if idx != -1 {
				f.inThink = false
				f.buffer = f.buffer[idx+8:]
				continue
			}

			if len(f.buffer) > 7 {
				f.buffer = f.buffer[len(f.buffer)-7:]
			}
			break
		}

		idx := strings.IndexByte(f.buffer, '<')
		if idx == -1 {
			out.WriteString(f.buffer)
			f.buffer = ""
			break
		}

		if idx > 0 {
			out.WriteString(f.buffer[:idx])
			f.buffer = f.buffer[idx:]
		}

		endIdx := strings.IndexByte(f.buffer, '>')
		if endIdx == -1 {
			if len(f.buffer) > 30 {
				out.WriteByte('<')
				f.buffer = f.buffer[1:]
				continue
			}
			break
		}

		tag := f.buffer[:endIdx+1]

		if tag == "<think>" {
			f.inThink = true
			f.buffer = f.buffer[endIdx+1:]
			continue
		}

		if tag == "</tool_call>" || tag == "</function>" || tag == "</parameter>" {
			f.buffer = f.buffer[endIdx+1:]
			continue
		}

		if strings.HasPrefix(tag, "<function=") || strings.HasPrefix(tag, "<parameter=") {
			f.buffer = f.buffer[endIdx+1:]
			continue
		}

		out.WriteByte('<')
		f.buffer = f.buffer[1:]
	}

	return out.String()
}

func (f *streamFilter) Flush() string {
	if f.inThink {
		return ""
	}
	res := f.buffer
	f.buffer = ""
	return res
}

func (o *Orchestrator) streamResponse(w http.ResponseWriter, r *http.Request, sessionID string, finalMessages []openai.ChatCompletionMessage, tools []openai.Tool, sources []api.SourceInfo) (string, []openai.ToolCall, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("streaming unsupported", "err", fmt.Errorf("response writer does not support flushing"))
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return "", nil, errors.New("streaming unsupported")
	}

	defer func() {
		if rcv := recover(); rcv != nil {
			slog.Error("panic in streamResponse", "panic", rcv)
			errorBytes, _ := json.Marshal(map[string]string{
				"error": "Internal error occurred",
			})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(errorBytes))
			flusher.Flush()
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sourcesBytes, _ := json.Marshal(sources)
	fmt.Fprintf(w, "event: sources\ndata: %s\n\n", string(sourcesBytes))
	flusher.Flush()

	filter := &streamFilter{}

	onDelta := func(content, reasoning string) error {
		if reasoning != "" {
			dataMap := map[string]string{"reasoning_content": reasoning}
			dataBytes, _ := json.Marshal(dataMap)
			fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
			flusher.Flush()
		}

		if content != "" {
			filteredContent := filter.Process(content)
			if filteredContent != "" {
				dataMap := map[string]string{"content": filteredContent}
				dataBytes, _ := json.Marshal(dataMap)
				fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
				flusher.Flush()
			}
		}
		return nil
	}

	assistantContent, _, toolCalls, err := o.generateResponse(r.Context(), finalMessages, tools, onDelta)
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			slog.Info("stream cancelled", "reason", ctxErr, "session_id", sessionID)
			return assistantContent, toolCalls, err
		}

		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return assistantContent, toolCalls, err
	}

	flushed := filter.Flush()
	if flushed != "" {
		dataMap := map[string]string{"content": flushed}
		dataBytes, _ := json.Marshal(dataMap)
		fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
		flusher.Flush()
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	return assistantContent, toolCalls, nil
}
