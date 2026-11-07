package attachments

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"go-monolith/internal/api"
)

// maxBodyBytes bounds a request: the content is JSON text, and a client may write
// every non-ASCII letter as a six-byte \u escape.
const maxBodyBytes = 3*MaxFileBytes + 8<<10

// RegisterRoutes adds the attachment endpoints to a mux whose requests already carry
// the user (api.UserMiddleware):
//
//	POST   /chat/attachments        {"session_id", "name", "content"} attach a file
//	GET    /chat/attachments?session_id=...   the files of a conversation (no content)
//	DELETE /chat/attachments/{id}   remove a file
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /chat/attachments", s.addHandler)
	mux.HandleFunc("GET /chat/attachments", s.listHandler)
	mux.HandleFunc("DELETE /chat/attachments/{id}", s.deleteHandler)
}

type addRequest struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Content   string `json:"content"`
}

func (s *Service) addHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	api.SetJSONReadDeadline(w)
	var req addRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, ErrTooLarge.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	added, err := s.Add(r.Context(), userID, req.SessionID, req.Name, req.Content)
	switch {
	case err == nil:
		status := http.StatusOK
		if added.Created {
			status = http.StatusCreated
		}
		writeJSON(w, status, added)
	case errors.Is(err, ErrTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, ErrTooMany), errors.Is(err, ErrSessionFull):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, ErrBadSession), errors.Is(err, ErrBadName), errors.Is(err, ErrEmpty), errors.Is(err, ErrBinary):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		slog.ErrorContext(r.Context(), "failed to attach a file", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (s *Service) listHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	list, err := s.List(r.Context(), userID, r.URL.Query().Get("session_id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, list)
	case errors.Is(err, ErrBadSession):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		slog.ErrorContext(r.Context(), "failed to list the attachments", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (s *Service) deleteHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid attachment id", http.StatusBadRequest)
		return
	}

	switch err := s.Delete(r.Context(), userID, id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "attachment not found", http.StatusNotFound)
	default:
		slog.ErrorContext(r.Context(), "failed to delete an attachment", "attachment_id", id, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
