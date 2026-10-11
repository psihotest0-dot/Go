package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go-monolith/internal/db"
	"go-monolith/internal/jobs"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/sqlc-dev/pqtype"
)

type API struct {
	db *db.Database
}

func New(db *db.Database) *API {
	return &API{db: db}
}

func (a *API) HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}

func getUserID(r *http.Request) (uuid.UUID, error) {
	userIDStr := r.Header.Get("X-User-ID")
	if userIDStr == "" {
		return uuid.Nil, errors.New("missing X-User-ID header")
	}
	return uuid.Parse(userIDStr)
}

func (a *API) CreateMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Role      string `json:"role"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	msg, err := a.db.Queries.CreateMessage(r.Context(), db.CreateMessageParams{
		UserID:    userID,
		SessionID: req.SessionID,
		Role:      req.Role,
		Content:   req.Content,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msg)
}

func (a *API) GetMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "missing session_id parameter", http.StatusBadRequest)
		return
	}

	msgs, err := a.db.Queries.GetMessagesBySession(r.Context(), db.GetMessagesBySessionParams{
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

func (a *API) CreateMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var req struct {
		Content         string                `json:"content"`
		Status          string                `json:"status"`
		SourceMessageID uuid.NullUUID         `json:"source_message_id"`
		Importance      int32                 `json:"importance"`
		Metadata        pqtype.NullRawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Status == "" {
		req.Status = "candidate"
	}

	mem, err := a.db.CreateMemoryWithCheck(r.Context(), db.CreateMemoryParams{
		UserID:          userID,
		Content:         req.Content,
		Status:          req.Status,
		SourceMessageID: req.SourceMessageID,
		Importance:      req.Importance,
		Metadata:        req.Metadata,
	})

	if errors.Is(err, db.ErrMemoryExists) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict) // 409 Conflict
		json.NewEncoder(w).Encode(mem)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(mem)
}

func (a *API) GetMemoriesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	mems, err := a.db.Queries.GetMemories(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if mems == nil {
		mems = []db.Memory{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mems)
}

func (a *API) MemoryByIDHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	idStr := parts[2] // /memories/{id}
	memoryID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid memory id", http.StatusBadRequest)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Content         string                `json:"content"`
			Status          string                `json:"status"`
			Importance      int32                 `json:"importance"`
			Metadata        pqtype.NullRawMessage `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		mem, err := a.db.UpdateMemoryWithCheck(r.Context(), db.UpdateMemoryParams{
			ID:         memoryID,
			UserID:     userID,
			Content:    req.Content,
			Status:     req.Status,
			Importance: req.Importance,
			Metadata:   req.Metadata,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mem)

	case http.MethodDelete:
		err := a.db.Queries.DeleteMemory(r.Context(), db.DeleteMemoryParams{
			ID:     memoryID,
			UserID: userID,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) CreateFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, err := getUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	// Limit request size to 10MB
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "file too large or invalid multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file in request", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Calculate SHA256 Hash
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	fileHash := hex.EncodeToString(hasher.Sum(nil))

	// Reset file pointer
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	// Get mimetype
	fileHeader := make([]byte, 512)
	if _, err := file.Read(fileHeader); err != nil && err != io.EOF {
		http.Error(w, "failed to read file header", http.StatusInternalServerError)
		return
	}

	mimeType := http.DetectContentType(fileHeader)

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	// Save to DB (Status pending)
	dbFile, err := a.db.Queries.CreateIngestedFile(r.Context(), db.CreateIngestedFileParams{
		UserID:   userID,
		FileName: header.Filename,
		FileHash: fileHash,
		MimeType: mimeType,
		Status:   "pending",
	})
	if err != nil {
		http.Error(w, "failed to save file record", http.StatusInternalServerError)
		return
	}

	// Save file physically
	err = os.MkdirAll("storage", os.ModePerm)
	if err != nil {
		http.Error(w, "failed to create storage directory", http.StatusInternalServerError)
		return
	}

	dstPath := filepath.Join("storage", dbFile.ID.String()+filepath.Ext(header.Filename))
	dst, err := os.Create(dstPath)
	if err != nil {
		http.Error(w, "failed to save file locally", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		http.Error(w, "failed to save file locally", http.StatusInternalServerError)
		return
	}

	// Enqueue Job
	_, err = river.ClientFromContext[pgx.Tx](r.Context()).Insert(r.Context(), jobs.IngestFileJobArgs{
		FileID: dbFile.ID,
	}, nil)

	if err != nil {
		// Log error, but file is saved.
		http.Error(w, "failed to enqueue job", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(dbFile)
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

	// Make sure it belongs to the user
	file, err := a.db.Queries.GetIngestedFile(r.Context(), fileID)
	if err != nil || file.UserID != userID {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	// Delete from DB (document_chunks cascade on delete)
	err = a.db.Queries.DeleteIngestedFile(r.Context(), fileID)
	if err != nil {
		http.Error(w, "failed to delete file from database", http.StatusInternalServerError)
		return
	}

	// Remove physical file
	// We check for any extension, but mostly we appended the original ext
	matches, err := filepath.Glob(filepath.Join("storage", file.ID.String()+".*"))
	if err == nil {
		for _, m := range matches {
			os.Remove(m)
		}
	}
	// Fallback in case there was no extension
	os.Remove(filepath.Join("storage", file.ID.String()))

	w.WriteHeader(http.StatusNoContent)
}
