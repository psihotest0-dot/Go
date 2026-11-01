package proposals

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"go-monolith/internal/api"
)

// RegisterRoutes adds the proposal endpoints to a mux whose requests already
// carry the user (api.UserMiddleware):
//
//	GET  /proposals               open proposals of the user
//	POST /proposals/{id}/approve  apply a proposal
//	POST /proposals/{id}/reject   drop a proposal
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /proposals", s.listHandler)
	mux.HandleFunc("POST /proposals/{id}/approve", s.decideHandler(s.Approve))
	mux.HandleFunc("POST /proposals/{id}/reject", s.decideHandler(s.Reject))
}

func (s *Service) listHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	list, err := s.Pending(r.Context(), userID)
	if err != nil {
		slog.Error("failed to list proposals", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Service) decideHandler(decide func(ctx context.Context, userID uuid.UUID, id int64) (Proposal, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.Error(w, "invalid proposal id", http.StatusBadRequest)
			return
		}

		p, err := decide(r.Context(), userID, id)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, p)
		case errors.Is(err, ErrNotFound):
			http.Error(w, "proposal not found", http.StatusNotFound)
		case errors.Is(err, ErrNotPending):
			http.Error(w, "proposal already decided", http.StatusConflict)
		case errors.Is(err, ErrTooManyRules):
			// Not "already decided": the proposal stays open, and deleting a rule
			// lets the user approve it again.
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		default:
			slog.Error("failed to decide a proposal", "id", id, "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
