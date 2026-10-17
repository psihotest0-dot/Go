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
	"go-monolith/internal/llm"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"
)

type API struct {
	db       *db.Database
	embedder llm.Embedder
}

func New(db *db.Database, embedder llm.Embedder) *API {
	return &API{db: db, embedder: embedder}
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
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Role      string `json:"role"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("invalid request body", slog.Any("error", err), slog.String("path", r.URL.Path))
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
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		slog.Warn("missing session_id parameter", slog.String("path", r.URL.Path))
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
				sources := []SourceInfo{}
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
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
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
		slog.Warn("invalid request body", slog.Any("error", err), slog.String("path", r.URL.Path))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Status == "" {
		req.Status = "candidate"
	}

	var newEmbedding pgvector.Vector
	var newEmbeddingModel string

	if strings.TrimSpace(req.Content) != "" {
		vectors, err := a.embedder.EmbedTexts(r.Context(), []string{req.Content})
		if err != nil {
			slog.Error("failed to generate embedding", slog.Any("error", err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(vectors) == 0 {
			slog.Error("embedder returned no vectors")
			http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
			return
		}
		if len(vectors[0]) == 0 {
			slog.Error("embedder returned empty vector")
			http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
			return
		}
		newEmbedding = pgvector.NewVector(vectors[0])
		newEmbeddingModel = "paraphrase-multilingual-MiniLM-L12-v2"
	}

	mem, err := a.db.CreateMemoryWithCheck(r.Context(), db.CreateMemoryParams{
		UserID:          userID,
		Content:         req.Content,
		Status:          req.Status,
		SourceMessageID: req.SourceMessageID,
		Importance:      req.Importance,
		Metadata:        req.Metadata,
		Embedding:       newEmbedding,
		EmbeddingModel:  newEmbeddingModel,
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
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
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
		slog.Warn("invalid path", slog.String("path", r.URL.Path))
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	idStr := parts[2] // /memories/{id}
	memoryID, err := uuid.Parse(idStr)
	if err != nil {
		slog.Warn("invalid memory id", slog.String("id", idStr), slog.String("path", r.URL.Path))
		http.Error(w, "invalid memory id", http.StatusBadRequest)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
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
			slog.Warn("invalid request body", slog.Any("error", err), slog.String("path", r.URL.Path))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if strings.TrimSpace(req.Content) == "" {
			http.Error(w, "content cannot be empty", http.StatusBadRequest)
			return
		}

		existingMem, err := a.db.Queries.GetMemoryByID(r.Context(), db.GetMemoryByIDParams{
			ID:     memoryID,
			UserID: userID,
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "memory not found", http.StatusNotFound)
			} else {
				slog.Error("failed to get existing memory", slog.Any("error", err))
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}

		newContentHash := db.GenerateHash(req.Content)
		var newEmbedding *pgvector.Vector
		var newEmbeddingModel sql.NullString

		if newContentHash != existingMem.ContentHash {
			vectors, err := a.embedder.EmbedTexts(r.Context(), []string{req.Content})
			if err != nil {
				slog.Error("failed to generate embedding", slog.Any("error", err))
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(vectors) == 0 {
				slog.Error("embedder returned no vectors")
				http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
				return
			}
			if len(vectors[0]) == 0 {
				slog.Error("embedder returned empty vector")
				http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
				return
			}
			vec := pgvector.NewVector(vectors[0])
			newEmbedding = &vec
			newEmbeddingModel = sql.NullString{String: "paraphrase-multilingual-MiniLM-L12-v2", Valid: true}
		}

		mem, err := a.db.UpdateMemoryWithCheck(r.Context(), db.UpdateMemoryParams{
			ID:             memoryID,
			UserID:         userID,
			Content:        sql.NullString{String: req.Content, Valid: true},
			Status:         sql.NullString{String: req.Status, Valid: true},
			Importance:     sql.NullInt32{Int32: req.Importance, Valid: true},
			Metadata:       req.Metadata,
			Embedding:      newEmbedding,
			EmbeddingModel: newEmbeddingModel,
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
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
