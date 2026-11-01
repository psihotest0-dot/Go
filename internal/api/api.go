package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/llm"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/sqlc-dev/pqtype"
)

// maxSnippetRunes caps the source snippet returned with a message.
const maxSnippetRunes = 600

// truncateRunes cuts s to at most max runes, appending "..." when it cut. Cutting
// by bytes would split a multi-byte character, which is every Cyrillic letter.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
}

type API struct {
	db       *db.Database
	embedder llm.Embedder
	// uploadLimit is the upload size cap in bytes; zero means maxUploadBytes.
	uploadLimit int64
}

func New(db *db.Database, embedder llm.Embedder) *API {
	return &API{db: db, embedder: embedder}
}

// WithUploadLimit sets the largest accepted upload in bytes (MAX_UPLOAD_MB).
func (a *API) WithUploadLimit(n int64) *API {
	a.uploadLimit = n
	return a
}

func (a *API) uploadLimitBytes() int64 {
	if a.uploadLimit > 0 {
		return a.uploadLimit
	}
	return maxUploadBytes
}

func (a *API) HealthzHandler(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Pool.Ping(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("Service Unavailable"))
		return
	}
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
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := validateMessageInput(req.SessionID, req.Role, req.Content); err != nil {
		slog.Warn("invalid message payload", slog.Any("error", err), slog.String("path", r.URL.Path))
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
		writeInternalError(w, "failed to create message", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(toMessageDTO(msg))
}

const (
	// defaultMessagesLimit is used when the client asks for no specific page
	// size. GetMessagesBySession always requires a LIMIT, so leaving it at the
	// zero value would silently return an empty page for every session.
	defaultMessagesLimit = 50
	// maxMessagesLimit caps the page size so a single request cannot pull the
	// complete history of a large session out of the database.
	maxMessagesLimit = 200
)

// parseMessagesLimit resolves the "limit" query parameter into a usable page
// size, falling back to defaultMessagesLimit when it is absent.
func parseMessagesLimit(raw string) (int32, error) {
	if raw == "" {
		return defaultMessagesLimit, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return 0, errors.New("limit must be a positive integer")
	}
	if limit > maxMessagesLimit {
		limit = maxMessagesLimit
	}
	return int32(limit), nil
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

	limit, err := parseMessagesLimit(r.URL.Query().Get("limit"))
	if err != nil {
		slog.Warn("invalid limit parameter", slog.Any("error", err), slog.String("path", r.URL.Path))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A session id is optional because a client that starts a fresh session on
	// every load has no way of naming the conversation it wants to restore,
	// and refusing the request left users with what looked like lost history
	// even though the rows were still there. Without an id the most recent
	// messages the user owns are returned; each one still carries its
	// session_id, so conversations remain distinguishable.
	var msgs []db.Message
	if sessionID == "" {
		msgs, err = a.db.Queries.GetRecentMessagesByUser(r.Context(), db.GetRecentMessagesByUserParams{
			UserID: userID,
			Limit:  limit,
		})
	} else {
		msgs, err = a.db.Queries.GetMessagesBySession(r.Context(), db.GetMessagesBySessionParams{
			UserID:    userID,
			SessionID: sessionID,
			Limit:     limit,
		})
	}
	if err != nil {
		writeInternalError(w, "failed to get messages", err)
		return
	}

	// Both queries return newest first because that is what a LIMIT page needs.
	// A transcript is rendered oldest first, so reverse before building the
	// response instead of relying on every client to sort it back.
	slices.Reverse(msgs)

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
		chunks, chunkErr := a.db.Queries.GetDocumentChunksWithFile(r.Context(), db.GetDocumentChunksWithFileParams{
			ChunkIds: allChunkIDs,
			UserID:   userID,
		})
		if chunkErr == nil {
			for _, chunk := range chunks {
				chunksMap[chunk.ID] = chunk
			}
		} else {
			slog.Error("failed to get document chunks with file", slog.Any("error", chunkErr))
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
						if chunk.ChunkType == "concept_summary" {
							sourceType = "concept"
						} else if strings.Contains(snippet, "[source: \"ocr\"]") {
							sourceType = "ocr"
							snippet = strings.ReplaceAll(snippet, "[source: \"ocr\"]", "")
						}
						snippet = truncateRunes(strings.TrimSpace(snippet), maxSnippetRunes)

						normalizedScore := c.Score
						scoreType := ""
						if sourceType == "text" || sourceType == "ocr" {
							normalizedScore = (c.Score / 0.033) * 100
							scoreType = "rrf"
						} else {
							normalizedScore = c.Score * 100
							scoreType = "cosine"
						}

						sources = append(sources, SourceInfo{
							ChunkID:        chunk.ID,
							FileName:       chunk.FileName,
							ContentSnippet: snippet,
							Score:          normalizedScore,
							ScoreType:      scoreType,
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
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Status == "" {
		req.Status = "candidate"
	}

	if err := validateMemoryInput(req.Content, req.Status, req.Importance); err != nil {
		slog.Warn("invalid memory payload", slog.Any("error", err), slog.String("path", r.URL.Path))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	contentHash := db.GenerateHash(req.Content)
	existingMem, err := a.db.Queries.GetMemoryByHash(r.Context(), db.GetMemoryByHashParams{
		UserID:      userID,
		ContentHash: contentHash,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeInternalError(w, "failed to get memory by hash", err)
		return
	}
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict) // 409 Conflict
		json.NewEncoder(w).Encode(toMemoryDTO(existingMem))
		return
	}

	var newEmbedding pgvector.Vector
	var newEmbeddingModel string

	if strings.TrimSpace(req.Content) != "" {
		vectors, embedErr := a.embedder.EmbedTexts(r.Context(), []string{req.Content}, false)
		if embedErr != nil {
			writeInternalError(w, "failed to generate embedding", embedErr)
			return
		}
		if len(vectors) == 0 {
			slog.Error("embedder returned no vectors")
			http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
			return
		}
		if len(vectors[0]) != config.VectorDimension {
			slog.Error("embedder returned vector with invalid dimension", slog.Int("dim", len(vectors[0])))
			http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
			return
		}
		newEmbedding = pgvector.NewVector(vectors[0])
		newEmbeddingModel = config.DefaultEmbedModel
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
		writeInternalError(w, "failed to create memory", err)
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
		writeInternalError(w, "failed to get memories", err)
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
		if !decodeJSON(w, r, &req) {
			return
		}

		if err := validateMemoryInput(req.Content, req.Status, req.Importance); err != nil {
			slog.Warn("invalid memory payload", slog.Any("error", err), slog.String("path", r.URL.Path))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		existingMem, dbErr := a.db.Queries.GetMemoryByID(r.Context(), db.GetMemoryByIDParams{
			ID:     memoryID,
			UserID: userID,
		})
		if dbErr != nil {
			if errors.Is(dbErr, sql.ErrNoRows) {
				http.Error(w, "memory not found", http.StatusNotFound)
			} else {
				writeInternalError(w, "failed to get existing memory", dbErr)
			}
			return
		}

		newContentHash := db.GenerateHash(req.Content)
		var newEmbedding *pgvector.Vector
		var newEmbeddingModel sql.NullString

		if newContentHash != existingMem.ContentHash {
			vectors, embedErr := a.embedder.EmbedTexts(r.Context(), []string{req.Content}, false)
			if embedErr != nil {
				writeInternalError(w, "failed to generate embedding", embedErr)
				return
			}
			if len(vectors) == 0 {
				slog.Error("embedder returned no vectors")
				http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
				return
			}
			if len(vectors[0]) != config.VectorDimension {
				slog.Error("embedder returned vector with invalid dimension", slog.Int("dim", len(vectors[0])))
				http.Error(w, "failed to generate valid embedding", http.StatusInternalServerError)
				return
			}
			vec := pgvector.NewVector(vectors[0])
			newEmbedding = &vec
			newEmbeddingModel = sql.NullString{String: config.DefaultEmbedModel, Valid: true}
		}

		mem, dbErr := a.db.UpdateMemoryWithCheck(r.Context(), db.UpdateMemoryParams{
			ID:             memoryID,
			UserID:         userID,
			Content:        sql.NullString{String: req.Content, Valid: true},
			Status:         sql.NullString{String: req.Status, Valid: true},
			Importance:     sql.NullInt32{Int32: req.Importance, Valid: true},
			Metadata:       req.Metadata,
			Embedding:      newEmbedding,
			EmbeddingModel: newEmbeddingModel,
		})
		if errors.Is(dbErr, db.ErrMemoryExists) {
			http.Error(w, "Memory already exists", http.StatusConflict)
			return
		} else if dbErr != nil {
			writeInternalError(w, "failed to update memory", dbErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toMemoryDTO(mem))

	case http.MethodDelete:
		err = a.db.Queries.DeleteMemory(r.Context(), db.DeleteMemoryParams{
			ID:     memoryID,
			UserID: userID,
		})
		if err != nil {
			writeInternalError(w, "failed to delete memory", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
