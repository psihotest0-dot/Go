package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"go-monolith/internal/db"
)

// RuleDTO is a global rule as the clients see it. Kind is "rule" for an instruction
// of the user and "style" for a note on how to talk with him (plan item P42).
type RuleDTO struct {
	ID        int32     `json:"id"`
	Kind      string    `json:"kind"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RegisterRuleRoutes adds the global rule endpoints to a mux whose requests
// already carry the user:
//
//	GET    /rules       the rules of the user
//	DELETE /rules/{id}  remove one rule
//
// A rule is created by confirming a proposal (see internal/proposals), never
// directly here.
func (a *API) RegisterRuleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /rules", a.ListRulesHandler)
	mux.HandleFunc("DELETE /rules/{id}", a.DeleteRuleHandler)
}

// ListRulesHandler returns every rule and style note of the user, newest first.
func (a *API) ListRulesHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.WarnContext(r.Context(), "unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rules, err := a.db.Queries.ListGlobalRules(r.Context(), userID)
	if err != nil {
		writeInternalError(w, r, "failed to list global rules", err)
		return
	}

	dtos := make([]RuleDTO, 0, len(rules))
	for _, rule := range rules {
		dtos = append(dtos, RuleDTO{
			ID:        rule.ID,
			Kind:      rule.Kind,
			Key:       rule.Key,
			Value:     rule.Value,
			Enabled:   rule.Enabled,
			UpdatedAt: rule.UpdatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(dtos); err != nil {
		slog.ErrorContext(r.Context(), "failed to write rules", slog.Any("error", err))
	}
}

// DeleteRuleHandler removes one rule of the user. A rule that does not exist
// (or belongs to someone else) is a 404.
func (a *API) DeleteRuleHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.WarnContext(r.Context(), "unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if err != nil || id < 1 {
		http.Error(w, "invalid rule id", http.StatusBadRequest)
		return
	}

	deleted, err := a.db.Queries.DeleteGlobalRule(r.Context(), db.DeleteGlobalRuleParams{
		ID:     int32(id),
		UserID: userID,
	})
	if err != nil {
		writeInternalError(w, r, "failed to delete a global rule", err)
		return
	}
	if deleted == 0 {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}

	slog.Info("global rule deleted", slog.Int64("id", id), slog.String("user_id", userID.String()))
	w.WriteHeader(http.StatusNoContent)
}
