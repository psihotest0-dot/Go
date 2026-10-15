package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
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

func buildSystemPrompt(rules []string, ragChunks []string) string {
	var b strings.Builder
	b.WriteString(`You are a helpful assistant.
You have access to long-term memory tools: search_memories, create_memory and save_global_rule.
Rules:
- Call search_memories FIRST when asked about past context or personal details.
- Call create_memory ONLY when the user explicitly shares new personal facts or preferences.
- Call save_global_rule ONLY when the user explicitly gives you an instruction on how to behave generally.
- NEVER claim you have remembered or saved something unless the tool call succeeded.

`)

	if len(rules) > 0 {
		b.WriteString("ЗАДЪЛЖИТЕЛНИ ГЛОБАЛНИ ПРАВИЛА (имат приоритет над всичко останало):\n")
		for _, r := range rules {
			b.WriteString("- " + r + "\n")
		}
		b.WriteString("\n")
	}

	if len(ragChunks) > 0 {
		b.WriteString("Контекст от векторната памет:\n" + strings.Join(ragChunks, "\n\n"))
	}

	return b.String()
}

// HandleStreamChat handles incoming chat messages and streams the response.
func (o *Orchestrator) HandleStreamChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("invalid request body", slog.Any("error", err), slog.String("path", r.URL.Path))
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" || req.Content == "" {
		slog.Warn("missing session_id or content", slog.String("path", r.URL.Path))
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
		SourceChunkIds: pqtype.NullRawMessage{
			RawMessage: []byte("[]"),
			Valid:      true,
		},
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

	// Extract sources for chunks
	sources := []api.SourceInfo{}
	var chunkIDs []uuid.UUID
	if err == nil {
		for _, m := range memories {
			if m.SourceType == "chunk" {
				chunkIDs = append(chunkIDs, m.ID)
			} else if m.SourceType == "memory" {
				sources = append(sources, api.SourceInfo{
					ChunkID:        m.ID,
					FileName:       "Памет (Memory)",
					ContentSnippet: m.Content,
					Score:          m.Score,
					SourceType:     "memory",
				})
			}
		}
	}

	if len(chunkIDs) > 0 {
		chunks, err := o.db.Queries.GetDocumentChunksWithFile(r.Context(), chunkIDs)
		if err == nil {
			for _, chunk := range chunks {
				// Map score
				var score float64
				for _, m := range memories {
					if m.ID == chunk.ID {
						score = m.Score
						break
					}
				}

				snippet := chunk.Text
				sourceType := "text"
				if strings.Contains(snippet, "[source: \"ocr\"]") {
					sourceType = "ocr"
					snippet = strings.ReplaceAll(snippet, "[source: \"ocr\"]", "")
				}
				snippet = strings.TrimSpace(snippet)
				if len(snippet) > 600 {
					snippet = snippet[:600] + "..."
				}

				sources = append(sources, api.SourceInfo{
					ChunkID:        chunk.ID,
					FileName:       chunk.FileName,
					ContentSnippet: snippet,
					Score:          score,
					SourceType:     sourceType,
				})
			}

			// Deduplicate based on ChunkID
			seen := make(map[uuid.UUID]bool)
			var dedupedSources []api.SourceInfo
			for _, s := range sources {
				if !seen[s.ChunkID] {
					seen[s.ChunkID] = true
					dedupedSources = append(dedupedSources, s)
				}
			}
			sources = dedupedSources

			// Sort by score descending using sort.Slice
			sort.Slice(sources, func(i, j int) bool {
				return sources[i].Score > sources[j].Score
			})
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

	// 3. Fetch Global Rules
	rules, err := o.db.Queries.GetGlobalRules(r.Context(), userID)
	if err != nil {
		slog.Error("failed to get global rules", "err", err)
		http.Error(w, fmt.Sprintf("failed to get global rules: %v", err), http.StatusInternalServerError)
		return
	}

	// 4. Construct System/Context Message
	systemContent := buildSystemPrompt(rules, contextTexts)

	systemMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemContent,
	}

	// 5. Fetch Message History (including the one we just saved)
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
		msg := openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		if msg.Role == "assistant" && msg.Content == "" && len(msg.ToolCalls) == 0 {
			continue
		}
		historyMessages = append(historyMessages, msg)
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
	// Define JSON schemas for tools
	tools := []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "create_memory",
				Description: "Save a new personal fact or preference about the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The explicit fact about the user in 3rd person.",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"description": "The importance of the fact from 1 to 10.",
						},
					},
					"required": []string{"content"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "search_memories",
				Description: "Search for past context or personal details about the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The text to search for.",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "The maximum number of results to return (default 5, max 20).",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "save_global_rule",
				Description: "Save a new global rule or preference (e.g., 'always speak in Bulgarian'). Overwrites existing rule with same key.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{
							"type":        "string",
							"description": "The short key for the rule (e.g., 'language', 'tone').",
						},
						"value": map[string]interface{}{
							"type":        "string",
							"description": "The rule instruction value.",
						},
					},
					"required": []string{"key", "value"},
				},
			},
		},
	}

	chatReq := openai.ChatCompletionRequest{
		Messages: finalMessages,
		Stream:   true,
		Tools:    tools,
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

	sourcesBytes, _ := json.Marshal(sources)
	fmt.Fprintf(w, "event: sources\ndata: %s\n\n", string(sourcesBytes))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Check if Bulgarian language is strictly required by any rule
	requiresBulgarian := false
	for _, rule := range rules {
		ruleLower := strings.ToLower(rule)
		if strings.Contains(ruleLower, "български") || strings.Contains(ruleLower, "bulgarian") {
			requiresBulgarian = true
			break
		}
	}

	var fullAssistantMessage strings.Builder
	var toolCalls []openai.ToolCall

	// Bonus: Bulgarian language enforcement vars
	var initialBuffer strings.Builder
	bufferMode := requiresBulgarian
	charCount := 0
	cyrillicCount := 0
	langCheckDone := false
	failedLangCheck := false

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
			delta := resp.Choices[0].Delta

			// Accumulate ToolCalls
			if len(delta.ToolCalls) > 0 {
				for _, tc := range delta.ToolCalls {
					// We need to merge tool calls based on Index
					if tc.Index != nil {
						idx := *tc.Index
						for len(toolCalls) <= idx {
							toolCalls = append(toolCalls, openai.ToolCall{})
						}

						if tc.ID != "" {
							toolCalls[idx].ID = tc.ID
						}
						if tc.Type != "" {
							toolCalls[idx].Type = tc.Type
						}
						if tc.Function.Name != "" {
							toolCalls[idx].Function.Name = tc.Function.Name
						}
						if tc.Function.Arguments != "" {
							toolCalls[idx].Function.Arguments += tc.Function.Arguments
						}
					}
				}
			}

			// Stream back only the content
			chunk := delta.Content
			if chunk != "" {
				fullAssistantMessage.WriteString(chunk)

				if bufferMode && !langCheckDone {
					initialBuffer.WriteString(chunk)
					for _, r := range chunk {
						// Only count letters
						if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= 'а' && r <= 'я') || (r >= 'А' && r <= 'Я') {
							charCount++
							if (r >= 'а' && r <= 'я') || (r >= 'А' && r <= 'Я') {
								cyrillicCount++
							}
						}
					}

					// We have enough letters to judge
					if charCount >= 50 {
						langCheckDone = true
						ratio := float64(cyrillicCount) / float64(charCount)
						if ratio < 0.60 {
							// Failed the language check. Cancel stream and fallback.
							failedLangCheck = true
							break // Break the inner loop, stream will close
						} else {
							// Passed. Flush the buffer.
							dataMap := map[string]string{"content": initialBuffer.String()}
							dataBytes, _ := json.Marshal(dataMap)
							fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
							if f, ok := w.(http.Flusher); ok {
								f.Flush()
							}
						}
					}
				} else {
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
		}
		if failedLangCheck {
			break
		}
	}

	// Handle the case where the message ended before 50 letters were accumulated
	if bufferMode && !langCheckDone && !failedLangCheck {
		if charCount > 0 {
			ratio := float64(cyrillicCount) / float64(charCount)
			if ratio < 0.60 {
				failedLangCheck = true
			}
		}

		if !failedLangCheck && initialBuffer.Len() > 0 {
			// Passed. Flush the buffer.
			dataMap := map[string]string{"content": initialBuffer.String()}
			dataBytes, _ := json.Marshal(dataMap)
			fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}

	if failedLangCheck {
		// Close the current stream early explicitly, since we're breaking out
		stream.Close()

		// 7.1 Language Fallback Stream
		finalMessages = append(finalMessages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: "Отговаряй САМО на български език!",
		})

		chatReqFallback := openai.ChatCompletionRequest{
			Messages: finalMessages,
			Stream:   true,
			Tools:    tools, // still pass tools just in case
		}

		streamFb, err := o.llmClient.ChatStream(r.Context(), chatReqFallback)
		if err != nil {
			slog.Error("llm fallback stream failed", "err", err)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		defer streamFb.Close()

		fullAssistantMessage.Reset()

		for {
			respFb, err := streamFb.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				break
			}
			if len(respFb.Choices) > 0 {
				chunk := respFb.Choices[0].Delta.Content
				if chunk != "" {
					fullAssistantMessage.WriteString(chunk)

					dataMap := map[string]string{"content": chunk}
					dataBytes, _ := json.Marshal(dataMap)
					fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			}
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Save Assistant Message
		contentToSave := fullAssistantMessage.String()
		var dbSources []api.DBChunkInfo
		for _, s := range sources {
			dbSources = append(dbSources, api.DBChunkInfo{
				ChunkID: s.ChunkID,
				Score:   s.Score,
			})
		}
		dbSourcesBytes, _ := json.Marshal(dbSources)

		_, err = o.db.Queries.CreateMessage(context.Background(), db.CreateMessageParams{
			UserID:    userID,
			SessionID: req.SessionID,
			Role:      "assistant",
			Content:   contentToSave,
			SourceChunkIds: pqtype.NullRawMessage{
				RawMessage: dbSourcesBytes,
				Valid:      true,
			},
		})
		if err != nil {
			fmt.Printf("failed to save assistant fallback message: %v\n", err)
		}
		return
	}

	// Execute tool calls if any
	if len(toolCalls) > 0 {
		// Append the assistant's tool call message
		finalMessages = append(finalMessages, openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			Content:   fullAssistantMessage.String(),
			ToolCalls: toolCalls,
		})

		for _, tc := range toolCalls {
			if tc.Function.Name != "" {
				var args map[string]interface{}
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				}

				res, err := o.tools.ExecuteTool(r.Context(), tc.Function.Name, args)

				var contentStr string
				if err != nil {
					slog.Error("tool execution failed", "tool", tc.Function.Name, "err", err)
					contentStr = fmt.Sprintf("Error: %v", err)
				} else {
					slog.Info("tool execution succeeded", "tool", tc.Function.Name, "result", res)
					resBytes, _ := json.Marshal(res)
					contentStr = string(resBytes)
				}

				finalMessages = append(finalMessages, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					Content:    contentStr,
					ToolCallID: tc.ID,
				})
			}
		}

		// Second LLM Call
		chatReq2 := openai.ChatCompletionRequest{
			Messages: finalMessages,
			Stream:   true,
			Tools:    tools, // still pass tools just in case
		}

		stream2, err := o.llmClient.ChatStream(r.Context(), chatReq2)
		if err != nil {
			slog.Error("llm second stream failed", "err", err)
			// At this point we might have already sent some chunks, so just end it
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		defer stream2.Close()

		for {
			resp2, err := stream2.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				break
			}
			if len(resp2.Choices) > 0 {
				chunk := resp2.Choices[0].Delta.Content
				if chunk != "" {
					fullAssistantMessage.WriteString(chunk)

					dataMap := map[string]string{"content": chunk}
					dataBytes, _ := json.Marshal(dataMap)
					fmt.Fprintf(w, "data: %s\n\n", string(dataBytes))
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
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
	contentToSave := fullAssistantMessage.String()
	// If it was just a tool call with no final content generated, format it
	if contentToSave == "" && len(toolCalls) > 0 {
		tcBytes, _ := json.Marshal(toolCalls)
		contentToSave = fmt.Sprintf("ToolCalls: %s", string(tcBytes))
	}

	var dbSources []api.DBChunkInfo
	for _, s := range sources {
		dbSources = append(dbSources, api.DBChunkInfo{
			ChunkID: s.ChunkID,
			Score:   s.Score,
		})
	}
	dbSourcesBytes, _ := json.Marshal(dbSources)

	_, err = o.db.Queries.CreateMessage(context.Background(), db.CreateMessageParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Role:      "assistant",
		Content:   contentToSave,
		SourceChunkIds: pqtype.NullRawMessage{
			RawMessage: dbSourcesBytes,
			Valid:      true,
		},
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
