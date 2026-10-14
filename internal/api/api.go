package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go-monolith/internal/db"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
)

type API struct {
	db *db.Database
}

func New(db *db.Database) *API {
	return &API{db: db}
}

func (a *API) HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}

func (a *API) CreateMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Role      string `json:"role"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	msg, err := a.db.Queries.CreateMessage(r.Context(), db.CreateMessageParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Role:      req.Role,
		Content:   req.Content,
	})
	if err != nil {
		slog.Error("failed to create message", slog.Any("error", err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toMessageDTO(msg))
}

func (a *API) GetMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "missing session_id parameter", http.StatusBadRequest)
		return
	}

	msgs, err := a.db.Queries.GetMessagesBySession(r.Context(), db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		slog.Error("failed to get messages by session", slog.Any("error", err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Fetch all necessary chunks for sources
	var allChunkIDs []uuid.UUID
	for _, m := range msgs {
		if m.SourceChunkIds.Valid {
			var dbChunks []DBChunkInfo
			if err := json.Unmarshal(m.SourceChunkIds.RawMessage, &dbChunks); err == nil {
				for _, c := range dbChunks {
					allChunkIDs = append(allChunkIDs, c.ChunkID)
				}
			}
		}
	}

	chunksMap := make(map[uuid.UUID]db.GetDocumentChunksWithFileRow)
	if len(allChunkIDs) > 0 {
		chunks, err := a.db.Queries.GetDocumentChunksWithFile(r.Context(), allChunkIDs)
		if err == nil {
			for _, chunk := range chunks {
				chunksMap[chunk.ID] = chunk
			}
		} else {
			slog.Error("failed to get document chunks with file", slog.Any("error", err))
		}
	}

	dtos := make([]MessageDTO, 0, len(msgs))
	for _, m := range msgs {
		dto := toMessageDTO(m)
		if m.SourceChunkIds.Valid {
			var dbChunks []DBChunkInfo
			if err := json.Unmarshal(m.SourceChunkIds.RawMessage, &dbChunks); err == nil {
				var sources []SourceInfo
				for _, c := range dbChunks {
					if chunk, ok := chunksMap[c.ChunkID]; ok {
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

						sources = append(sources, SourceInfo{
							ChunkID:        chunk.ID,
							FileName:       chunk.FileName,
							ContentSnippet: snippet,
							Score:          c.Score,
							SourceType:     sourceType,
						})
					}
				}
				dto.Sources = sources
			}
		}
		dtos = append(dtos, dto)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dtos)
}

func (a *API) CreateMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Content         string                `json:"content"`
		Status          string                `json:"status"`
		SourceMessageID uuid.NullUUID         `json:"source_message_id"`
		Importance      int32                 `json:"importance"`
		Metadata        pqtype.NullRawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Status == "" {
		req.Status = "candidate"
	}

	mem, err := a.db.CreateMemoryWithCheck(r.Context(), db.CreateMemoryParams{
		UserID:          userID,
		Content:         req.Content,
		Status:          req.Status,
		SourceMessageID: req.SourceMessageID,
		Importance:      req.Importance,
		Metadata:        req.Metadata,
	})

	if errors.Is(err, db.ErrMemoryExists) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict) // 409 Conflict
		json.NewEncoder(w).Encode(toMemoryDTO(mem))
		return
	} else if err != nil {
		slog.Error("failed to create memory", slog.Any("error", err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(toMemoryDTO(mem))
}

func (a *API) GetMemoriesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	statusFilter := r.URL.Query().Get("status")
	var statusParam sql.NullString
	if statusFilter != "" {
		statusParam = sql.NullString{String: statusFilter, Valid: true}
	}

	mems, err := a.db.Queries.GetMemories(r.Context(), db.GetMemoriesParams{
		UserID: userID,
		Status: statusParam,
	})
	if err != nil {
		slog.Error("failed to get memories", slog.Any("error", err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	dtos := make([]MemoryDTO, 0, len(mems))
	for _, m := range mems {
		if strings.TrimSpace(m.Content) != "" {
			dtos = append(dtos, toMemoryDTO(m))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dtos)
}

func (a *API) MemoryByIDHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	idStr := parts[2] // /memories/{id}
	memoryID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid memory id", http.StatusBadRequest)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Content    string                `json:"content"`
			Status     string                `json:"status"`
			Importance int32                 `json:"importance"`
			Metadata   pqtype.NullRawMessage `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		mem, err := a.db.UpdateMemoryWithCheck(r.Context(), db.UpdateMemoryParams{
			ID:         memoryID,
			UserID:     userID,
			Content:    req.Content,
			Status:     req.Status,
			Importance: req.Importance,
			Metadata:   req.Metadata,
		})
		if err != nil {
			slog.Error("failed to update memory", slog.Any("error", err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toMemoryDTO(mem))

	case http.MethodDelete:
		err := a.db.Queries.DeleteMemory(r.Context(), db.DeleteMemoryParams{
			ID:     memoryID,
			UserID: userID,
		})
		if err != nil {
			slog.Error("failed to delete memory", slog.Any("error", err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
