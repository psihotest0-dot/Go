package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/retrieval"
)

// Orchestrator orchestrates the main agent pipeline.
type Orchestrator struct {
	db        *db.Database
	retrieval *retrieval.RetrievalService
	llmClient llm.LLMClient
	tools     ToolRegistry
}

// NewOrchestrator creates a new Orchestrator.
func NewOrchestrator(database *db.Database, r *retrieval.RetrievalService, l llm.LLMClient, t ToolRegistry) *Orchestrator {
	return &Orchestrator{
		db:        database,
		retrieval: r,
		llmClient: l,
		tools:     t,
	}
}

// ChatRequest defines the expected payload for the stream chat endpoint.
type ChatRequest struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}

// HandleStreamChat handles incoming chat messages and streams the response.
func (o *Orchestrator) HandleStreamChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" || req.Content == "" {
		http.Error(w, "session_id and content are required", http.StatusBadRequest)
		return
	}

	// 1. Save user message to database (to have it in history/log right away)
	// We'll just save it to the DB so it's recorded. But we don't necessarily have to
	// include it in the `GetMessagesBySession` if we append it manually.
	// Actually, let's append it manually and save both at the end, or save user now.
	// We will save user now.
	_, err := o.db.Queries.CreateMessage(r.Context(), db.CreateMessageParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Role:      "user",
		Content:   req.Content,
	})
	if err != nil {
		slog.Error("failed to save user message", "err", err)
		http.Error(w, fmt.Sprintf("failed to save user message: %v", err), http.StatusInternalServerError)
		return
	}

	// 2. Retrieval: hybrid search for context
	memories, err := o.retrieval.SearchSimilar(r.Context(), userID, req.Content, 5)
	var contextTexts []string
	var memoryIDs []string
	if err == nil {
		for _, m := range memories {
			contextTexts = append(contextTexts, m.Content)
			memoryIDs = append(memoryIDs, m.ID.String())
		}
	}

	// Log trace
	traceDetails, _ := json.Marshal(map[string]interface{}{
		"query":      req.Content,
		"memory_ids": memoryIDs,
	})
	_, _ = o.db.Queries.CreateAgentTrace(r.Context(), db.CreateAgentTraceParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Step:      "search",
		Details: pqtype.NullRawMessage{
			RawMessage: traceDetails,
			Valid:      true,
		},
	})

	// 3. Construct System/Context Message
	systemContent := "You are a helpful assistant."
	if len(contextTexts) > 0 {
		systemContent += "\n\nRelevant Context:\n" + strings.Join(contextTexts, "\n\n")
	}

	systemMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemContent,
	}

	// 4. Fetch Message History (including the one we just saved)
	dbMsgs, err := o.db.Queries.GetMessagesBySession(r.Context(), db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: req.SessionID,
	})
	if err != nil {
		slog.Error("failed to get messages", "err", err)
		http.Error(w, fmt.Sprintf("failed to get messages: %v", err), http.StatusInternalServerError)
		return
	}

	var historyMessages []openai.ChatCompletionMessage
	for _, m := range dbMsgs {
		historyMessages = append(historyMessages, openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: m.Content,
		})
	}

	// 5. Pruning Logic (Sliding Window based on character count)
	maxChars := 2000
	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)

	// Keep taking from history (newest to oldest) until we hit the char limit
	var reversedKeep []openai.ChatCompletionMessage
	currentChars := utf8.RuneCountInString(systemMessage.Content)

	for i := len(historyMessages) - 1; i >= 0; i-- {
		msgLen := utf8.RuneCountInString(historyMessages[i].Content)
		if currentChars+msgLen > maxChars && len(reversedKeep) > 0 {
			break
		}
		reversedKeep = append(reversedKeep, historyMessages[i])
		currentChars += msgLen
	}

	// Revert the kept messages back to chronological order
	for i := len(reversedKeep) - 1; i >= 0; i-- {
		finalMessages = append(finalMessages, reversedKeep[i])
	}

	// Log trace for prune
	tracePruneDetails, _ := json.Marshal(map[string]interface{}{
		"total_messages": len(historyMessages) + 1,
		"pruned_to":      len(finalMessages),
		"total_chars":    currentChars,
	})
	_, _ = o.db.Queries.CreateAgentTrace(r.Context(), db.CreateAgentTraceParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Step:      "prune",
		Details: pqtype.NullRawMessage{
			RawMessage: tracePruneDetails,
			Valid:      true,
		},
	})

	// 6. Call LLM
	chatReq := openai.ChatCompletionRequest{
		Messages: finalMessages,
		Stream:   true,
	}

	stream, err := o.llmClient.ChatStream(r.Context(), chatReq)
	if err != nil {
		slog.Error("llm stream failed", "err", err)
		http.Error(w, fmt.Sprintf("llm stream failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer stream.Close()

	// 7. Stream Response SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var fullAssistantMessage strings.Builder

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// write error as SSE comment or data
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			} else {
				slog.Error("streaming unsupported", "err", fmt.Errorf("response writer does not support flushing"))
				http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
				return
			}
			break
		}

		if len(resp.Choices) > 0 {
			chunk := resp.Choices[0].Delta.Content
			fullAssistantMessage.WriteString(chunk)

			// SSE format
			dataMap := map[string]string{"content": chunk}
			dataBytes, _ := json.Marshal(dataMap)
			fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			} else {
				slog.Error("streaming unsupported", "err", fmt.Errorf("response writer does not support flushing"))
				http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
				return
			}
		}
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	} else {
		slog.Error("streaming unsupported", "err", fmt.Errorf("response writer does not support flushing"))
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 8. Save Assistant Message
	_, err = o.db.Queries.CreateMessage(context.Background(), db.CreateMessageParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Role:      "assistant",
		Content:   fullAssistantMessage.String(),
	})
	if err != nil {
		// Log but cannot return error to client since we already streamed
		fmt.Printf("failed to save assistant message: %v\n", err)
	}

	// Trace Stream End
	traceStreamDetails, _ := json.Marshal(map[string]interface{}{
		"response_length": fullAssistantMessage.Len(),
	})
	_, _ = o.db.Queries.CreateAgentTrace(context.Background(), db.CreateAgentTraceParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Step:      "stream_end",
		Details: pqtype.NullRawMessage{
			RawMessage: traceStreamDetails,
			Valid:      true,
		},
	})
}
