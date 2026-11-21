package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
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

// RegisterSessionRoutes adds the chat session endpoints to a mux whose requests already
// carry the user:
//
//	DELETE /chat/sessions/{id}  delete the conversation (204; also when there is none)
//	GET    /telegram/sessions   the Telegram conversations of the user
func (a *API) RegisterSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /chat/sessions/{id}", a.DeleteSessionHandler)
	mux.HandleFunc("GET /telegram/sessions", a.ListTelegramSessionsHandler)
}

// TelegramSessionPrefix begins the session id of a conversation with a Telegram chat.
const TelegramSessionPrefix = "tg-"

// TelegramSessionID is the session id of the conversation with a Telegram chat. The bot writes to
// it and the app opens it, so both take the id from here.
func TelegramSessionID(chatID int64) string {
	return TelegramSessionPrefix + strconv.FormatInt(chatID, 10)
}

// IsTelegramSession reports whether sessionID names a conversation with a Telegram chat.
func IsTelegramSession(sessionID string) bool {
	return strings.HasPrefix(sessionID, TelegramSessionPrefix)
}

// ParseTelegramSession is the reverse of TelegramSessionID: the chat id of "tg-<chat id>", and
// false for any other text (including "tg-" with something else than a number after it).
func ParseTelegramSession(sessionID string) (int64, bool) {
	rest, ok := strings.CutPrefix(sessionID, TelegramSessionPrefix)
	if !ok {
		return 0, false
	}
	chatID, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || TelegramSessionID(chatID) != sessionID {
		return 0, false // "+5", "05", " 5" and the like are not what the bot writes
	}
	return chatID, true
}

// TelegramSessionDTO is one Telegram conversation as the app lists it.
type TelegramSessionDTO struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

// TelegramSessionsResponse is the body of GET /telegram/sessions.
type TelegramSessionsResponse struct {
	Sessions []TelegramSessionDTO `json:"sessions"`
}

// ListTelegramSessionsHandler lists the Telegram chats tied to the user (/link) as conversations
// the app can open next to its own: "Telegram", then "Telegram 2" and so on in the order of the
// chat ids. A user with no Telegram link gets an empty list.
func (a *API) ListTelegramSessionsHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.WarnContext(r.Context(), "unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	chats, err := a.db.Queries.ListTelegramChatsByUser(r.Context(), userID)
	if err != nil {
		writeInternalError(w, r, "failed to list the telegram chats", err)
		return
	}

	out := TelegramSessionsResponse{Sessions: make([]TelegramSessionDTO, 0, len(chats))}
	for i, chatID := range chats {
		name := "Telegram"
		if i > 0 {
			name += " " + strconv.Itoa(i+1)
		}
		out.Sessions = append(out.Sessions, TelegramSessionDTO{SessionID: TelegramSessionID(chatID), Name: name})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		slog.ErrorContext(r.Context(), "failed to write the telegram sessions", slog.Any("error", err))
	}
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
