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

const (
	// streamFilterMaxBuffered bounds how many bytes streamFilter holds back
	// while waiting for a tag to complete (longest supported tag plus slack).
	streamFilterMaxBuffered = 30
	// streamFilterThinkTail is how many bytes are kept while inside a <think>
	// block so a split closing marker is still detected.
	streamFilterThinkTail = 7
	// streamFilterThinkClose is the marker that ends a thinking block.
	streamFilterThinkClose = "</think>"
)

type streamFilter struct {
	inThink bool
	buffer  string
}

// Process appends chunk to the internal buffer and returns the content that is
// safe to forward to the client. The buffer stays bounded: only a short tail is
// retained, so an unterminated tag cannot grow memory without limit.
func (f *streamFilter) Process(chunk string) string {
	f.buffer += chunk
	var out strings.Builder

	for len(f.buffer) > 0 {
		if f.inThink {
			idx := strings.Index(f.buffer, streamFilterThinkClose)
			if idx != -1 {
				f.inThink = false
				f.buffer = f.buffer[idx+len(streamFilterThinkClose):]
				continue
			}

			if len(f.buffer) > streamFilterThinkTail {
				f.buffer = f.buffer[len(f.buffer)-streamFilterThinkTail:]
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
			// No closing '>' anywhere in the buffer, so this cannot become a
			// tag. Flush the excess in one pass instead of dropping a single
			// byte per iteration, which made long runs of '<' quadratic.
			if len(f.buffer) > streamFilterMaxBuffered {
				cut := len(f.buffer) - streamFilterMaxBuffered
				out.WriteString(f.buffer[:cut])
				f.buffer = f.buffer[cut:]
			}
			break
		}

		tag := f.buffer[:endIdx+1]

		if tag == "<think>" {
			f.inThink = true
			f.buffer = f.buffer[endIdx+1:]
			continue
		}

		if tag == "<tool_call>" || tag == "</tool_call>" || tag == "</function>" || tag == "</parameter>" {
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
		f.buffer = ""
		return ""
	}
	res := f.buffer
	f.buffer = ""
	return res
}

// Reset releases the buffered content so the filter does not retain it once the
// stream is over, whether it finished or failed.
func (f *streamFilter) Reset() {
	f.inThink = false
	f.buffer = ""
}

// sanitizeAssistantText applies the live stream's filter to a complete answer:
// <think> blocks (an unterminated one included) and tool-call markup are removed,
// so the stored and Telegram text equals what the web client was shown.
func sanitizeAssistantText(s string) string {
	f := &streamFilter{}
	return strings.TrimSpace(f.Process(s) + f.Flush())
}

func (o *Orchestrator) streamResponse(w http.ResponseWriter, r *http.Request, sessionID string, finalMessages []openai.ChatCompletionMessage, tools []openai.Tool, sources []api.SourceInfo) (assistantContent string, toolCalls []openai.ToolCall, err error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("streaming unsupported", "err", fmt.Errorf("response writer does not support flushing"))
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return "", nil, errors.New("streaming unsupported")
	}

	filter := &streamFilter{}

	// A panic must be reported as an error instead of a nil one, otherwise the
	// caller treats the truncated answer as a success. The filter is released
	// on every path so it does not keep buffered content alive.
	defer func() {
		if rcv := recover(); rcv != nil {
			slog.Error("panic in streamResponse", "panic", rcv, "session_id", sessionID)
			writeSSEError(w, flusher, "Internal error occurred")
			assistantContent, toolCalls = "", nil
			err = fmt.Errorf("panic while streaming response: %v", rcv)
		}
		filter.Reset()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sourcesBytes, _ := json.Marshal(sources)
	fmt.Fprintf(w, "event: sources\ndata: %s\n\n", string(sourcesBytes))
	flusher.Flush()

	onDelta := func(content, reasoning string) error {
		// Stop as soon as the client is gone: otherwise the LLM keeps
		// streaming into a dead connection and the answer keeps growing for
		// nothing.
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return ctxErr
		}

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

	assistantContent, _, toolCalls, err = o.generateResponse(r.Context(), finalMessages, tools, onDelta)
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			slog.Info("stream cancelled", "reason", ctxErr, "session_id", sessionID)
			return assistantContent, toolCalls, err
		}

		// err can embed the LLM URL or a provider response body; the client gets
		// a generic message and a request_id to find the details in the log.
		reqID, _ := r.Context().Value(api.RequestIDKey).(string)
		slog.Error("generation failed", "error", err, "session_id", sessionID, "request_id", reqID)
		writeSSEError(w, flusher, streamErrorMessage(reqID))
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

// streamErrorMessage is the client-facing text for a failed generation.
func streamErrorMessage(requestID string) string {
	if requestID == "" {
		return "Failed to generate a response. Please try again."
	}
	return "Failed to generate a response. Please try again. (request_id: " + requestID + ")"
}

// writeSSEError sends a terminal SSE error frame and flushes it.
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, message string) {
	errorBytes, marshalErr := json.Marshal(map[string]string{"error": message})
	if marshalErr != nil {
		// The payload only contains strings, so this is not expected.
		slog.Error("failed to marshal stream error payload", "err", marshalErr)
		return
	}
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(errorBytes))
	flusher.Flush()
}
