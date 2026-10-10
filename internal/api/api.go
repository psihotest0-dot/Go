package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "OK"})
}

func getUserID(r *http.Request) (uuid.UUID, error) {
	userIDStr := r.Header.Get("X-User-ID")
	if userIDStr == "" {
		return uuid.Nil, errors.New("missing X-User-ID header")
	}
	return uuid.Parse(userIDStr)
}

func (a *API) CreateMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msg)
}

func (a *API) GetMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

func (a *API) CreateMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
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
		json.NewEncoder(w).Encode(mem)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(mem)
}

func (a *API) GetMemoriesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	mems, err := a.db.Queries.GetMemories(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if mems == nil {
		mems = []db.Memory{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mems)
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

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Content         string                `json:"content"`
			Status          string                `json:"status"`
			Importance      int32                 `json:"importance"`
			Metadata        pqtype.NullRawMessage `json:"metadata"`
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mem)

	case http.MethodDelete:
		err := a.db.Queries.DeleteMemory(r.Context(), db.DeleteMemoryParams{
			ID:     memoryID,
			UserID: userID,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
