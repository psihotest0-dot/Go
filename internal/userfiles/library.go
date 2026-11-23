package userfiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"go-monolith/internal/api"
)

// The two stores of the owner (2026-11-23): the folder of Lili's files, which he reads, changes
// and deletes, and the library, where what was added is not touched again. A file of the folder
// he likes goes into the library with one button:
//
//	POST /workspace/files/{name}/library   the text of the file becomes a document of the library
//
// It is the road of a document the server fetched itself (api.LibraryAdder: stored, refused when
// the library has the same text, queued for the ingest). The file stays in the folder.

// LibraryAdder puts a document into the library of a user (api.LibraryAdder).
type LibraryAdder interface {
	Add(ctx context.Context, userID uuid.UUID, name string, src io.Reader) (api.AddedFile, error)
}

// WithLibrary lets the files of the folder be added to the library. Call it before RegisterRoutes;
// without it the route answers that the library is not there.
func (s *Store) WithLibrary(a LibraryAdder) *Store {
	s.library = a
	return s
}

type libraryResponse struct {
	FileID    uuid.UUID `json:"file_id"`
	FileName  string    `json:"file_name"`
	Duplicate bool      `json:"duplicate"`
	Size      int64     `json:"size"`
}

func (s *Store) libraryHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := requestUser(w, r)
	if !ok {
		return
	}
	if s.library == nil {
		http.Error(w, "the library cannot be reached from the files", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	text, _, err := s.Text(userID, name)
	if err != nil {
		fail(w, r, "read a file for the library", err)
		return
	}
	added, err := s.library.Add(r.Context(), userID, name, strings.NewReader(text))
	switch {
	case err == nil:
	case errors.Is(err, api.ErrAddTooLarge):
		http.Error(w, "the file is larger than what the library takes", http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, api.ErrAddEmpty):
		http.Error(w, "the file is empty", http.StatusBadRequest)
		return
	default:
		slog.ErrorContext(r.Context(), "failed to add a file of the folder to the library", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	status := http.StatusCreated
	if added.Duplicate {
		status = http.StatusOK
	}
	slog.InfoContext(r.Context(), "a file of the folder went into the library", "file_id", added.ID, "duplicate", added.Duplicate, "bytes", added.Size)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(libraryResponse{FileID: added.ID, FileName: added.Name, Duplicate: added.Duplicate, Size: added.Size})
}
