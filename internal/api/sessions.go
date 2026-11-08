package api

import (
	"log/slog"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"go-monolith/internal/db"
)

// maxSessionIDRunes matches the check on chat_attachments.session_id.
const maxSessionIDRunes = 128

// validSessionID: 1 to 128 characters, none of them a control character.
func validSessionID(id string) bool {
	if n := utf8.RuneCountInString(id); n < 1 || n > maxSessionIDRunes {
		return false
	}
	return !strings.ContainsFunc(id, unicode.IsControl)
}

// RegisterSessionRoutes adds the chat session endpoint to a mux whose requests already
// carry the user:
//
//	DELETE /chat/sessions/{id}  delete the conversation (204; also when there is none)
func (a *API) RegisterSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /chat/sessions/{id}", a.DeleteSessionHandler)
}

// DeleteSessionHandler deletes one conversation of the user for good: its messages, the
// rolling summary, the traces and the attached files. The user's memories stay (see
// db.Database.DeleteSession). A chat turn that is still being generated in that session
// can write its answer after the deletion; the clients stop deleting while one runs.
func (a *API) DeleteSessionHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.WarnContext(r.Context(), "unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sessionID := r.PathValue("id")
	if !validSessionID(sessionID) || strings.HasPrefix(sessionID, db.RetiredSessionPrefix) {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}

	res, err := a.db.DeleteSession(r.Context(), userID, sessionID)
	if err != nil {
		writeInternalError(w, r, "failed to delete a chat session", err)
		return
	}

	slog.InfoContext(r.Context(), "chat session deleted",
		slog.Int64("messages", res.Messages), slog.Int64("kept_for_memory_analysis", res.Retired),
		slog.Int64("attachments", res.Attachments), slog.Int64("traces", res.Traces), slog.Int64("summaries", res.Summaries))
	w.WriteHeader(http.StatusNoContent)
}
