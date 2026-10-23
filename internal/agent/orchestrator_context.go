package agent

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
)

func (o *Orchestrator) loadContext(ctx context.Context, userID uuid.UUID, req ChatRequest) (chatContext, []string, error) {
	var chatCtx chatContext
	var memoryIDs []string

	// Call new SearchAll method
	memories, chunks, errM := o.retrieval.SearchAll(ctx, userID, req.Content, 5)
	if errM != nil {
		slog.Warn("Failed to search all", "error", errM)
	}

	for _, m := range memories {
		chatCtx.Memories = append(chatCtx.Memories, m.Content)
		memoryIDs = append(memoryIDs, m.ID.String())
		chatCtx.Sources = append(chatCtx.Sources, api.SourceInfo{
			ChunkID:        m.ID,
			FileName:       "Памет (Memory)",
			ContentSnippet: m.Content,
			Score:          m.Score,
			SourceType:     "memory",
		})
	}

	for _, c := range chunks {
		chatCtx.Chunks = append(chatCtx.Chunks, c.Content)

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

		chatCtx.Sources = append(chatCtx.Sources, api.SourceInfo{
			ChunkID:        c.ID,
			FileName:       c.FileName,
			ContentSnippet: snippet,
			Score:          c.Score,
			SourceType:     sourceType,
		})
	}

	// Deduplicate based on ChunkID
	seen := make(map[uuid.UUID]bool)
	var dedupedSources []api.SourceInfo
	for _, s := range chatCtx.Sources {
		if !seen[s.ChunkID] {
			seen[s.ChunkID] = true
			dedupedSources = append(dedupedSources, s)
		}
	}
	chatCtx.Sources = dedupedSources

	// Sort by score descending
	sort.Slice(chatCtx.Sources, func(i, j int) bool {
		return chatCtx.Sources[i].Score > chatCtx.Sources[j].Score
	})

	// Fetch Global Rules
	rules, err := o.db.Queries.GetGlobalRules(ctx, userID)
	if err != nil {
		slog.Error("failed to get global rules", "err", err)
		return chatCtx, memoryIDs, err
	}
	chatCtx.Rules = rules

	// Fetch Top Memories
	topMemoriesDb, err := o.db.Queries.GetTopMemories(ctx, db.GetTopMemoriesParams{
		UserID: userID,
		Limit:  5,
	})
	if err != nil {
		slog.Error("failed to get top memories", "err", err)
	}
	for _, m := range topMemoriesDb {
		chatCtx.TopMemories = append(chatCtx.TopMemories, m.Content)
	}

	// Fetch Message History
	dbMsgs, err := o.db.Queries.GetMessagesBySession(ctx, db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Limit:     maxHistoryMessages,
	})
	if err != nil {
		slog.Error("failed to get messages", "err", err)
		return chatCtx, memoryIDs, err
	}

	// dbMsgs are returned in DESC order (newest first).
	// We need them in ASC order (oldest first) for the prompt history.
	for i := len(dbMsgs) - 1; i >= 0; i-- {
		m := dbMsgs[i]
		msg := openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		if msg.Role == "assistant" && msg.Content == "" && len(msg.ToolCalls) == 0 {
			continue
		}
		chatCtx.History = append(chatCtx.History, msg)
	}

	return chatCtx, memoryIDs, nil
}
