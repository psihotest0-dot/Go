package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"go-monolith/internal/taskplan"
)

// TaskPlanResponse is the answer of GET /task-plan: the newest plan of the conversation, or
// null when it has none.
type TaskPlanResponse struct {
	Plan *taskplan.Plan `json:"plan"`
}

// RegisterTaskPlanRoutes adds the read of the plan of a conversation to a mux whose requests
// already carry the user:
//
//	GET /task-plan?session_id=...  the newest plan of the conversation (or {"plan": null})
//
// The plan is made and changed by the assistant's tools only; the app only shows it.
func (a *API) RegisterTaskPlanRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /task-plan", a.TaskPlanHandler)
}

// TaskPlanHandler returns the newest plan of one conversation of the user, whatever became of
// it: a finished plan shows what was done. 400 without a usable session_id.
func (a *API) TaskPlanHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.WarnContext(r.Context(), "unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if !validSessionID(sessionID) {
		http.Error(w, "session_id must be 1 to 128 characters without control characters", http.StatusBadRequest)
		return
	}

	plan, err := taskplan.NewStore(a.db).Latest(r.Context(), userID, sessionID)
	if err != nil {
		writeInternalError(w, r, "failed to read the task plan", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(TaskPlanResponse{Plan: plan}); err != nil {
		slog.ErrorContext(r.Context(), "failed to write the task plan", slog.Any("error", err))
	}
}
