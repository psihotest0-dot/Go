package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"strings"

	"go-monolith/internal/db"
	"go-monolith/internal/jobs"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

func (a *API) CreateFileHandler(riverClient *river.Client[pgx.Tx]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		userID, err := getUserID(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		// Enforce size limit (e.g. 50MB)
		r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
		if err := r.ParseMultipartForm(50 << 20); err != nil {
			http.Error(w, "file too large or invalid multipart form", http.StatusBadRequest)
			return
		}

		file, handler, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// Read into memory for hash and mimetype (for files under 50MB this is ok, else temp file is better)
		// Since MaxBytesReader is used, reading all is safe up to 50MB.
		fileBytes, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "failed to read file", http.StatusInternalServerError)
			return
		}

		hash := sha256.Sum256(fileBytes)
		hashStr := hex.EncodeToString(hash[:])

		mtype := mimetype.Detect(fileBytes)
		mimeStr := mtype.String()

		fileSize := int64(len(fileBytes))

		// Check deduplication
		existingFile, err := a.db.Queries.GetIngestedFileByHash(r.Context(), db.GetIngestedFileByHashParams{
			UserID:   userID,
			FileHash: hashStr,
		})
		if err == nil && existingFile.ID != uuid.Nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict) // 409
			json.NewEncoder(w).Encode(existingFile)
			return
		}

		// Ensure storage dir
		os.MkdirAll("storage", 0755)
		localPath := filepath.Join("storage", fmt.Sprintf("%s_%s", uuid.New().String(), filepath.Base(handler.Filename)))

		err = os.WriteFile(localPath, fileBytes, 0644)
		if err != nil {
			http.Error(w, "failed to save file locally", http.StatusInternalServerError)
			return
		}

		ingestedFile, err := a.db.Queries.CreateIngestedFile(r.Context(), db.CreateIngestedFileParams{
			UserID:    userID,
			FileName:  handler.Filename,
			FileHash:  hashStr,
			MimeType:  mimeStr,
			FileSize:  fileSize,
			Status:    "pending",
			LocalPath: localPath,
		})
		if err != nil {
			http.Error(w, "failed to record file in database", http.StatusInternalServerError)
			return
		}

		if riverClient != nil {
			_, err = riverClient.Insert(r.Context(), jobs.IngestFileArgs{
				FileID: ingestedFile.ID,
			}, nil)
			if err != nil {
				// Log error, but file is saved
				fmt.Printf("failed to enqueue ingest job: %v\n", err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(ingestedFile)
	}
}

func (a *API) GetFilesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	filesDB, err := a.db.Queries.GetIngestedFiles(r.Context(), userID)
	if err != nil {
		http.Error(w, "failed to get files", http.StatusInternalServerError)
		return
	}

	// Initialize as empty slice to avoid null in JSON response
	files := []db.IngestedFile{}
	if len(filesDB) > 0 {
		files = filesDB
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (a *API) DeleteFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	idStr := parts[3] // /v1/files/{id}
	fileID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid file id", http.StatusBadRequest)
		return
	}

	file, err := a.db.Queries.GetIngestedFileByID(r.Context(), fileID)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	if file.UserID != userID {
		http.Error(w, "unauthorized access to file", http.StatusForbidden)
		return
	}

	// Delete from DB (document_chunks cascade on delete)
	err = a.db.Queries.DeleteIngestedFile(r.Context(), db.DeleteIngestedFileParams{
		ID:     fileID,
		UserID: userID,
	})
	if err != nil {
		http.Error(w, "failed to delete file from db", http.StatusInternalServerError)
		return
	}

	// Delete from local storage
	if file.LocalPath != "" {
		_ = os.Remove(file.LocalPath)
	}

	w.WriteHeader(http.StatusNoContent)
}
