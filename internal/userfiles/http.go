package userfiles

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"go-monolith/internal/api"
)

// RegisterRoutes adds the endpoints with which the user sees and clears the files the
// assistant keeps. They are not for the model, which has its own tools, and they are
// registered only when the file tools are on. The mux must carry the user
// (api.UserMiddleware):
//
//	GET    /workspace/files                the files and what they take
//	GET    /workspace/files/{name}         the text of a file; ?version=previous for the
//	                                       version a replacement took the place of
//	DELETE /workspace/files/{name}         remove a file and its previous version
//	POST   /workspace/files/{name}/library put the text of a file into the library (library.go)
func (s *Store) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /workspace/files", s.listHandler)
	mux.HandleFunc("GET /workspace/files/{name}", s.getHandler)
	mux.HandleFunc("DELETE /workspace/files/{name}", s.deleteHandler)
	mux.HandleFunc("POST /workspace/files/{name}/library", s.libraryHandler)
}

type listResponse struct {
	Files         []Info `json:"files"`
	TotalBytes    int64  `json:"total_bytes"`
	MaxFiles      int    `json:"max_files"`
	MaxFileBytes  int    `json:"max_file_bytes"`
	MaxTotalBytes int64  `json:"max_total_bytes"`
}

type fileResponse struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Content string `json:"content"`
	// Size and Modified are those of the current file; the previous version has neither.
	Size     int64      `json:"size,omitempty"`
	Modified *time.Time `json:"modified,omitempty"`
}

func requestUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return userID, ok
}

// fail answers a failed call: a bad name is the caller's, a missing file is a 404, the rest
// is the server's.
func fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var invalid *InvalidError
	switch {
	case errors.As(err, &invalid):
		http.Error(w, invalid.Reason, http.StatusBadRequest)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "no such file", http.StatusNotFound)
	default:
		slog.ErrorContext(r.Context(), "failed to "+what, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (s *Store) listHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := requestUser(w, r)
	if !ok {
		return
	}
	files, err := s.List(userID)
	if err != nil {
		fail(w, r, "list the user's files", err)
		return
	}
	_, total, err := s.Usage(userID)
	if err != nil {
		fail(w, r, "count the user's files", err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Files:         files,
		TotalBytes:    total,
		MaxFiles:      s.limits.MaxFiles,
		MaxFileBytes:  s.limits.MaxFileBytes,
		MaxTotalBytes: s.limits.MaxTotalBytes,
	})
}

func (s *Store) getHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := requestUser(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")

	if r.URL.Query().Get("version") == "previous" {
		text, err := s.Previous(userID, name)
		if err != nil {
			fail(w, r, "read a previous version", err)
			return
		}
		writeJSON(w, http.StatusOK, fileResponse{Name: name, Version: "previous", Content: text})
		return
	}

	text, info, err := s.Text(userID, name)
	if err != nil {
		fail(w, r, "read a file", err)
		return
	}
	writeJSON(w, http.StatusOK, fileResponse{Name: info.Name, Version: "current", Content: text, Size: info.Size, Modified: &info.Modified})
}

func (s *Store) deleteHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := requestUser(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.Delete(userID, name); err != nil {
		fail(w, r, "delete a file", err)
		return
	}
	slog.InfoContext(r.Context(), "a file was deleted by the user", "user_id", userID, "name", name)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
