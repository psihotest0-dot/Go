package bills

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"go-monolith/internal/api"
)

// RegisterRoutes adds POST /v1/bills: a multipart form with "file" (the PDF or the picture of a
// bill) and "session_id" (the conversation the answer goes into). The answer is
// {"message": "..."}: what was read and the question whether to keep it; the upload and the
// message are in the conversation too, so a reload shows them.
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/bills", s.uploadHandler)
}

func (s *Service) uploadHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(api.ParsedUserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxFileBytes+1<<20)
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "a multipart form with file and session_id is required", http.StatusBadRequest)
		return
	}
	var session, name string
	var data []byte
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "the file is too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "session_id":
			b, _ := io.ReadAll(io.LimitReader(part, 200))
			session = strings.TrimSpace(string(b))
		case "file":
			name = part.FileName()
			data, err = io.ReadAll(io.LimitReader(part, MaxFileBytes+1))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "the file is too large", http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, "invalid multipart form", http.StatusBadRequest)
				return
			}
		}
		_ = part.Close()
	}
	if session == "" || data == nil {
		http.Error(w, "file and session_id are required", http.StatusBadRequest)
		return
	}
	message, err := s.Read(r.Context(), userID, session, name, data)
	if err != nil {
		slog.ErrorContext(r.Context(), "a bill was read but not written into the conversation", "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}
