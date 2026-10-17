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
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sqlc-dev/pqtype"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"
	"go-monolith/internal/prompts"
	"go-monolith/internal/retrieval"
)

type sessionSummary struct {
	mu            sync.Mutex
	text          string
	lastDropCount int
	isGenerating  bool
}

// Orchestrator orchestrates the main agent pipeline.
type Orchestrator struct {
	db               *db.Database
	retrieval        *retrieval.RetrievalService
	llmClient        llm.LLMClient
	tools            ToolRegistry
	sessionSummaries sync.Map
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
	// Search Memories
	memories, errM := o.retrieval.SearchMemories(r.Context(), userID, req.Content, 5)
	var memoryTexts []string
	var memoryIDs []string
	sources := []api.SourceInfo{}
	if errM == nil {
		for _, m := range memories {
			memoryTexts = append(memoryTexts, m.Content)
			memoryIDs = append(memoryIDs, m.ID.String())
			sources = append(sources, api.SourceInfo{
				ChunkID:        m.ID,
				FileName:       "Памет (Memory)",
				ContentSnippet: m.Content,
				Score:          m.Score,
				SourceType:     "memory",
			})
		}
	}

	// Search Chunks
	chunks, errC := o.retrieval.SearchChunks(r.Context(), userID, req.Content, 5)
	var chunkTexts []string
	var chunkIDs []uuid.UUID
	if errC == nil {
		for _, c := range chunks {
			chunkTexts = append(chunkTexts, c.Content)
			chunkIDs = append(chunkIDs, c.ID)

			snippet := c.Content
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
				ChunkID:        c.ID,
				FileName:       c.FileName,
				ContentSnippet: snippet,
				Score:          c.Score,
				SourceType:     sourceType,
			})
		}
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

	// 4. Fetch Top Memories
	topMemoriesDb, err := o.db.Queries.GetTopMemories(r.Context(), db.GetTopMemoriesParams{
		UserID: userID,
		Limit:  5,
	})
	if err != nil {
		slog.Error("failed to get top memories", "err", err)
	}
	var topMemories []string
	for _, m := range topMemoriesDb {
		topMemories = append(topMemories, m.Content)
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

	// 6. Sliding Window and Rolling Summary
	var oldMessages []openai.ChatCompletionMessage

	// Keep taking from history (newest to oldest) until we hit the char limit
	maxChars := 12000
	var reversedKeep []openai.ChatCompletionMessage
	currentChars := 0

	for i := len(historyMessages) - 1; i >= 0; i-- {
		msgLen := utf8.RuneCountInString(historyMessages[i].Content)
		// Prune if char limit exceeded or more than 10 messages kept
		if (currentChars+msgLen > maxChars && len(reversedKeep) > 0) || len(reversedKeep) >= 10 {
			oldMessages = historyMessages[:i+1]
			break
		}
		reversedKeep = append(reversedKeep, historyMessages[i])
		currentChars += msgLen
	}

	// Revert the kept messages back to chronological order
	var finalHistory []openai.ChatCompletionMessage
	for i := len(reversedKeep) - 1; i >= 0; i-- {
		finalHistory = append(finalHistory, reversedKeep[i])
	}
	historyMessages = finalHistory

	droppedMessagesCount := len(oldMessages)
	var rollingSummary string

	v, _ := o.sessionSummaries.LoadOrStore(req.SessionID, &sessionSummary{})
	summaryState := v.(*sessionSummary)

	summaryState.mu.Lock()
	rollingSummary = summaryState.text
	lastDropCount := summaryState.lastDropCount
	isGenerating := summaryState.isGenerating
	if droppedMessagesCount > 0 && (droppedMessagesCount-lastDropCount) >= 6 && !isGenerating {
		summaryState.isGenerating = true
	} else {
		droppedMessagesCount = 0 // Don't generate
	}
	summaryState.mu.Unlock()

	if droppedMessagesCount > 0 {
		var newOlds []openai.ChatCompletionMessage
		if droppedMessagesCount > lastDropCount {
			newOlds = oldMessages[lastDropCount:]
		}

		go func(sessID string, olds []openai.ChatCompletionMessage, dropCount int, previousSummary string) {
			defer func() {
				v, ok := o.sessionSummaries.Load(sessID)
				if ok {
					state := v.(*sessionSummary)
					state.mu.Lock()
					state.isGenerating = false
					state.mu.Unlock()
				}
			}()

			if len(olds) == 0 {
				return
			}

			var oldTexts []string
			for _, m := range olds {
				oldTexts = append(oldTexts, fmt.Sprintf("%s: %s", m.Role, m.Content))
			}

			summaryPrompt, err := prompts.GlobalManager.Render("rolling_summary.tmpl", map[string]interface{}{
				"MessagesText":    strings.Join(oldTexts, "\n"),
				"PreviousSummary": previousSummary,
			})
			if err == nil {
				summaryReq := openai.ChatCompletionRequest{
					Messages: []openai.ChatCompletionMessage{
						{
							Role:    openai.ChatMessageRoleSystem,
							Content: summaryPrompt,
						},
					},
				}
				// Use pure Chat instead of ChatWithJSONSchema since we want a plain text summary
				summaryResp, sumErr := o.llmClient.Chat(context.Background(), summaryReq)
				if sumErr == nil && len(summaryResp.Choices) > 0 {
					if v, ok := o.sessionSummaries.Load(sessID); ok {
						state := v.(*sessionSummary)
						state.mu.Lock()
						state.text = summaryResp.Choices[0].Message.Content
						state.lastDropCount = dropCount
						state.mu.Unlock()
					}
				} else {
					slog.Error("failed to generate rolling summary", "err", sumErr)
				}
			} else {
				slog.Error("failed to render rolling summary prompt", "err", err)
			}
		}(req.SessionID, newOlds, droppedMessagesCount, rollingSummary)
	}

	// 7. Construct System/Context Message
	systemContent, err := prompts.GlobalManager.Render("chat_system.tmpl", map[string]interface{}{
		"Rules":            rules,
		"TopMemories":      topMemories,
		"RollingSummary":   rollingSummary,
		"RelevantMemories": memoryTexts,
		"Chunks":           chunkTexts,
	})
	if err != nil {
		slog.Error("failed to render system prompt", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	systemMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemContent,
	}

	var finalMessages []openai.ChatCompletionMessage
	finalMessages = append(finalMessages, systemMessage)
	finalMessages = append(finalMessages, historyMessages...)

	// Log trace for prune
	tracePruneDetails, _ := json.Marshal(map[string]interface{}{
		"total_messages": len(historyMessages) + 1,
		"pruned_to":      len(finalMessages),
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
				Name:        "search_documents",
				Description: "Search for context, information, or answers within the uploaded documents and files.",
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

		var secondCallText strings.Builder

		func() {
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
						secondCallText.WriteString(chunk)
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
		}()

		// Fallback third call if empty text after tool usage
		if strings.TrimSpace(secondCallText.String()) == "" {
			var toolResponses strings.Builder
			for _, tcMsg := range finalMessages {
				if tcMsg.Role == openai.ChatMessageRoleTool {
					toolResponses.WriteString(tcMsg.Content)
					toolResponses.WriteString("\n\n")
				}
			}

			fallbackFinalMessages := finalMessages
			fallbackFinalMessages = append(fallbackFinalMessages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: "Отговори на въпроса на потребителя въз основа на намерените документи по-долу.\n\n" + toolResponses.String(),
			})

			chatReqFallbackTool := chatReq2
			chatReqFallbackTool.Messages = fallbackFinalMessages
			chatReqFallbackTool.Tools = nil // Force text generation

			stream3, err := o.llmClient.ChatStream(r.Context(), chatReqFallbackTool)
			if err != nil {
				slog.Error("llm third stream failed", "err", err)
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			defer stream3.Close()

			for {
				resp3, err := stream3.Recv()
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
				if len(resp3.Choices) > 0 {
					chunk := resp3.Choices[0].Delta.Content
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
