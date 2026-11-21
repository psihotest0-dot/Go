package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
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
	// streamFilterThinkOpen and streamFilterThinkClose are the markers that start
	// and end a thinking block.
	streamFilterThinkOpen  = "<think>"
	streamFilterThinkClose = "</think>"
	// streamFilterCallOpen and streamFilterCallClose bound a tool call that Gemma 4 wrote
	// as text, <|tool_call>call:append_file{...}<tool_call|>. The server parses such a call
	// when the tool is on offer; when no tool is (the last round after the rounds for tools
	// ran out) the model writes it into the answer, and the user got the call itself.
	streamFilterCallOpen  = "<|tool_call>"
	streamFilterCallClose = "<tool_call|>"
	// streamFilterDSMLOpen starts the same thing in DeepSeek's own format, <｜｜DSML｜｜ calls>
	// <｜｜DSML｜｜ invoke name="search_documents">...: found 2026-11-14, when the last request
	// after the rounds for tools (no tool on offer) was answered with such a call in 5 of 79
	// turns. The bars are full-width (U+FF5C); no ordinary text has "<" right before one. The
	// open tag can be longer than the buffer for tags, so the start alone is enough.
	streamFilterDSMLOpen = "<｜"
)

// streamFilterDSMLClose are the ends of a call in DeepSeek's format (the two spellings seen).
var streamFilterDSMLClose = []string{"</｜｜DSML｜｜ calls>", "</｜DSML｜tool_calls>"}

// streamFilterCallTail is how many bytes are kept while inside such a block so that a
// split closing marker is still detected.
var streamFilterCallTail = longestMarker(append([]string{streamFilterCallClose}, streamFilterDSMLClose...)) - 1

func longestMarker(markers []string) int {
	longest := 0
	for _, m := range markers {
		longest = max(longest, len(m))
	}
	return longest
}

// firstMarker returns the earliest of the markers in text: where it starts and where it ends,
// or -1.
func firstMarker(text string, markers []string) (start, end int) {
	start = -1
	for _, m := range markers {
		if i := strings.Index(text, m); i != -1 && (start == -1 || i < start) {
			start, end = i, i+len(m)
		}
	}
	return start, end
}

type streamFilter struct {
	inThink bool
	// inCall: inside a tool call written as text; everything up to one of its closing markers
	// (callClose) is dropped.
	inCall    bool
	callClose []string
	// inRecord: the model began to copy the record of the tools (copiedToolRecord); nothing
	// after it is shown.
	inRecord bool
	buffer   string
}

// Process appends chunk to the internal buffer and returns the content that is
// safe to forward to the client. The buffer stays bounded: only a short tail is
// retained, so an unterminated tag cannot grow memory without limit.
func (f *streamFilter) Process(chunk string) string {
	f.buffer += chunk
	var out strings.Builder

scan:
	for len(f.buffer) > 0 {
		if f.inRecord {
			f.buffer = ""
			break
		}
		if f.inCall {
			if _, end := firstMarker(f.buffer, f.callClose); end != 0 {
				f.inCall = false
				f.buffer = f.buffer[end:]
				continue
			}

			if len(f.buffer) > streamFilterCallTail {
				f.buffer = f.buffer[len(f.buffer)-streamFilterCallTail:]
			}
			break
		}

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

		idx := strings.IndexAny(f.buffer, "<[")
		if idx == -1 {
			out.WriteString(f.buffer)
			f.buffer = ""
			break
		}

		if idx > 0 {
			out.WriteString(f.buffer[:idx])
			f.buffer = f.buffer[idx:]
		}

		if f.buffer[0] == '[' {
			switch {
			case strings.HasPrefix(f.buffer, copiedToolRecord):
				f.inRecord = true
				f.buffer = ""
			case strings.HasPrefix(copiedToolRecord, f.buffer):
				// It may still become the record: wait for the next chunk.
				break scan
			default:
				out.WriteByte('[')
				f.buffer = f.buffer[1:]
			}
			continue
		}

		if strings.HasPrefix(f.buffer, streamFilterDSMLOpen) {
			f.inCall, f.callClose = true, streamFilterDSMLClose
			f.buffer = f.buffer[len(streamFilterDSMLOpen):]
			continue
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

		if tag == streamFilterThinkOpen {
			f.inThink = true
			f.buffer = f.buffer[endIdx+1:]
			continue
		}

		if tag == streamFilterCallOpen {
			f.inCall, f.callClose = true, []string{streamFilterCallClose}
			f.buffer = f.buffer[endIdx+1:]
			continue
		}

		// A closing think marker outside a think block closes a block whose opening
		// tag the model server put into the prompt. The tag itself is never shown;
		// the reasoning before it was already sent and cannot be taken back here
		// (sanitizeAssistantText removes it from the stored answer).
		if tag == "<tool_call>" || tag == "</tool_call>" || tag == "</function>" || tag == "</parameter>" || tag == streamFilterThinkClose {
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
	if f.inThink || f.inCall || f.inRecord {
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
	f.inCall = false
	f.inRecord = false
	f.buffer = ""
}

// dropOrphanThinking removes everything up to a </think> that closes no <think>.
// The chat template of some models opens the block in the prompt, so the reply
// is "reasoning</think>answer" with no opening tag. Keeping the text before it
// stored a draft and a thought as part of the answer.
func dropOrphanThinking(s string) string {
	for {
		closeIdx := strings.Index(s, streamFilterThinkClose)
		if closeIdx == -1 {
			return s
		}
		if openIdx := strings.Index(s, streamFilterThinkOpen); openIdx != -1 && openIdx < closeIdx {
			return s
		}
		s = s[closeIdx+len(streamFilterThinkClose):]
	}
}

// sanitizeAssistantText applies the live stream's filter to a complete answer:
// <think> blocks (an unterminated one included) and tool-call markup are removed,
// so the stored and Telegram text equals what the web client was shown. The one
// difference is reasoning that ends in a </think> with no opening tag: the live
// stream has already sent it, but it is not stored.
func sanitizeAssistantText(s string) string {
	f := &streamFilter{}
	return strings.TrimSpace(f.Process(dropOrphanThinking(s)) + f.Flush())
}

// copiedToolRecord starts the note the server puts after an answer in the history (tool_log.go).
// The model reads it there and may copy it into a new answer (1 and 2 of 12 answers in the
// measurement of 2026-11-21); a record it writes itself is not one, so the filter shows nothing
// from where it starts.
const copiedToolRecord = "[System record"

// streamResponse streams one answer as Server-Sent Events. beforeDone (may be nil)
// gets the finished answer just before the final "[DONE]" frame and stores it: a
// client that reads the history, or sends the next message, as soon as the stream
// ends must find the answer there. It is not called when the generation fails.
func (o *Orchestrator) streamResponse(w http.ResponseWriter, r *http.Request, sessionID string, finalMessages []openai.ChatCompletionMessage, tools []openai.Tool, sources []api.SourceInfo, beforeDone func(content string, toolCalls []openai.ToolCall)) (assistantContent string, err error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.ErrorContext(r.Context(), "streaming unsupported", "err", fmt.Errorf("response writer does not support flushing"))
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return "", errors.New("streaming unsupported")
	}

	filter := &streamFilter{}

	// A panic must be reported as an error instead of a nil one, otherwise the
	// caller treats the truncated answer as a success. The filter is released
	// on every path so it does not keep buffered content alive.
	defer func() {
		if rcv := recover(); rcv != nil {
			slog.ErrorContext(r.Context(), "panic in streamResponse", "panic", rcv, "session_id", sessionID)
			writeSSEError(w, flusher, "Internal error occurred")
			assistantContent = ""
			err = fmt.Errorf("panic while streaming response: %v", rcv)
		}
		filter.Reset()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// The contract promises a list, "[]" when there are none; a nil slice would
	// be marshalled as "null".
	firstSources := sources
	if firstSources == nil {
		firstSources = []api.SourceInfo{}
	}
	sourcesBytes, _ := json.Marshal(firstSources)
	fmt.Fprintf(w, "event: sources\ndata: %s\n\n", string(sourcesBytes))
	flusher.Flush()

	// The passages the model finds with a tool while it works are not in that first
	// frame. They follow in a second "sources" frame, as soon as the text that is
	// based on them starts (or at the end), and only the ones the client does not
	// have yet; the client merges the frames. turn is nil outside a chat turn.
	turn := turnFrom(r.Context())
	announced := slices.Clone(sources)
	sendFoundSources := func() {
		fresh := withoutKnownSources(announced, turn.newSources())
		if len(fresh) == 0 {
			return
		}
		announced = append(announced, fresh...)
		freshBytes, _ := json.Marshal(fresh)
		fmt.Fprintf(w, "event: sources\ndata: %s\n\n", string(freshBytes))
		flusher.Flush()
	}

	onDelta := func(content, reasoning string) error {
		// Stop as soon as the client is gone: otherwise the LLM keeps
		// streaming into a dead connection and the answer keeps growing for
		// nothing.
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return ctxErr
		}
		sendFoundSources()

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

	assistantContent, toolCalls, err := o.generateResponse(r.Context(), finalMessages, tools, onDelta)
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			slog.Info("stream cancelled", "reason", ctxErr, "session_id", sessionID)
			return assistantContent, err
		}

		// err can embed the LLM URL or a provider response body; the client gets
		// a generic message and a request_id to find the details in the log.
		reqID, _ := r.Context().Value(api.RequestIDKey).(string)
		slog.Error("generation failed", "error", err, "session_id", sessionID, "request_id", reqID)
		writeSSEError(w, flusher, streamErrorMessage(reqID))
		return assistantContent, err
	}

	flushed := filter.Flush()
	if flushed != "" {
		dataMap := map[string]string{"content": flushed}
		dataBytes, _ := json.Marshal(dataMap)
		fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
		flusher.Flush()
	}

	// An answer that streamed nothing after the last tool call still owes the
	// client the passages that call found.
	sendFoundSources()

	// Writes the model asked for wait for the user; tell the client so it can ask.
	if made := turnFrom(r.Context()).madeProposals(); len(made) > 0 {
		proposalBytes, _ := json.Marshal(made)
		fmt.Fprintf(w, "event: proposals\ndata: %s\n\n", string(proposalBytes))
		flusher.Flush()
	}

	// Who answered, for the client to show; only while the paid model is configured.
	if frame := o.modelEvent(r.Context()); frame != "" {
		fmt.Fprint(w, frame)
		flusher.Flush()
	}

	if beforeDone != nil {
		beforeDone(assistantContent, toolCalls)
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	return assistantContent, nil
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
