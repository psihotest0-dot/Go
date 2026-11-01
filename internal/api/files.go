package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-monolith/internal/config"
	"go-monolith/internal/db"
	"go-monolith/internal/jobs"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// FilePrefixes lists the path prefixes the file endpoints are registered under.
// The documented API lives under /v1/files, while the chat UI calls /files next
// to /messages and /memories. Serving both keeps existing clients working and
// prevents the UI from hitting a 404 when it lists files.
var FilePrefixes = []string{"/v1/files", "/files"}

// categoryPathSuffix addresses the category sub-resource of a file.
const categoryPathSuffix = "/category"

var (
	// errInvalidFilePath marks a path that is not /[v1/]files/{id}[/category].
	errInvalidFilePath = errors.New("invalid file path")
	// errInvalidFileID marks a path whose id segment is not a UUID.
	errInvalidFileID = errors.New("invalid file id")
)

// RegisterFileRoutes wires the file collection and item endpoints onto mux
// under every prefix in FilePrefixes.
func (a *API) RegisterFileRoutes(mux *http.ServeMux, riverClient *river.Client[pgx.Tx]) {
	collection := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			a.CreateFileHandler(riverClient)(w, r)
		case http.MethodGet:
			a.GetFilesHandler(w, r)
		default:
			slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}

	item := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, categoryPathSuffix) {
			a.FileCategoryHandler(w, r)
			return
		}
		a.FileByIDHandler(w, r)
	}

	for _, prefix := range FilePrefixes {
		mux.HandleFunc(prefix, collection)
		mux.HandleFunc(prefix+"/", item)
	}
}

// parseFilePath extracts the file id from a files path and reports whether the
// path addresses the category sub-resource. Both the /v1/files/{id} and the
// /files/{id} forms are accepted.
func parseFilePath(path string) (uuid.UUID, bool, error) {
	var rest string
	found := false
	for _, prefix := range FilePrefixes {
		if trimmed, ok := strings.CutPrefix(path, prefix+"/"); ok {
			rest, found = trimmed, true
			break
		}
	}
	if !found {
		return uuid.Nil, false, errInvalidFilePath
	}

	rest, category := strings.CutSuffix(rest, categoryPathSuffix)
	if rest == "" || strings.Contains(rest, "/") {
		return uuid.Nil, false, errInvalidFilePath
	}

	fileID, err := uuid.Parse(rest)
	if err != nil {
		return uuid.Nil, false, errInvalidFileID
	}
	return fileID, category, nil
}

// parseFileRequest resolves the file id addressed by r and answers the request
// with 400 when the path is not a valid files path. The boolean result is false
// once a response has been written.
func parseFileRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool, bool) {
	fileID, category, err := parseFilePath(r.URL.Path)
	if err != nil {
		msg := "invalid path"
		if errors.Is(err, errInvalidFileID) {
			msg = "invalid file id"
		}
		slog.Warn(msg, slog.String("path", r.URL.Path))
		http.Error(w, msg, http.StatusBadRequest)
		return uuid.Nil, false, false
	}
	return fileID, category, true
}

// fileChunkCount returns how many embedded chunks belong to the file. Counting
// is best effort: a failure must not hide the file metadata the client asked
// for, but it is logged because a silent zero looks like "not embedded".
func (a *API) fileChunkCount(ctx context.Context, fileID, userID uuid.UUID) int64 {
	count, err := a.db.Queries.GetIngestedFileChunkCount(ctx, db.GetIngestedFileChunkCountParams{
		FileID: fileID,
		UserID: userID,
	})
	if err != nil {
		slog.Error("failed to count file chunks",
			slog.Any("error", err),
			slog.String("file_id", fileID.String()))
		return 0
	}
	return count
}

func (a *API) CreateFileHandler(riverClient *river.Client[pgx.Tx]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
		if !ok {
			slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// The file is streamed straight into uploadDir while it is hashed, so the
		// request body never has to fit in memory.
		upload, err := a.spoolUpload(w, r, a.uploadLimitBytes())
		if err != nil {
			var clientErr *clientError
			if errors.As(err, &clientErr) {
				if clientErr.status >= http.StatusInternalServerError {
					slog.Error("failed to accept upload", slog.Any("error", err), slog.String("path", r.URL.Path))
				} else {
					slog.Warn("rejected upload", slog.String("reason", clientErr.msg), slog.String("path", r.URL.Path))
				}
				http.Error(w, clientErr.msg, clientErr.status)
				return
			}
			writeInternalError(w, "failed to accept upload", err)
			return
		}

		// Every failure path below has to leave neither a row nor a file behind.
		committed := false
		defer func() {
			if !committed {
				upload.discard()
			}
		}()

		// Check deduplication
		existingFile, err := a.db.Queries.GetIngestedFileByHash(r.Context(), db.GetIngestedFileByHashParams{
			UserID:   userID,
			FileHash: upload.hash,
		})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeInternalError(w, "failed to check existing file by hash", err)
			return
		}
		if err == nil && existingFile.ID != uuid.Nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict) // 409
			json.NewEncoder(w).Encode(toIngestedFileDTO(existingFile, a.fileChunkCount(r.Context(), existingFile.ID, userID)))
			return
		}

		ingestedFile, err := a.db.Queries.CreateIngestedFile(r.Context(), db.CreateIngestedFileParams{
			UserID:    userID,
			FileName:  upload.fileName,
			FileHash:  upload.hash,
			MimeType:  upload.mimeType,
			FileSize:  upload.size,
			Status:    "pending",
			LocalPath: upload.path,
		})
		if err != nil {
			writeInternalError(w, "failed to record file in database", err)
			return
		}

		if riverClient != nil {
			_, jobErr := riverClient.Insert(r.Context(), jobs.IngestFileArgs{
				FileID: ingestedFile.ID,
			}, jobs.IngestInsertOpts())
			if jobErr != nil {
				// Without the job nothing would ever process the file, and a row
				// left in "pending" would make the client wait forever. Roll the
				// upload back instead of answering 201 for work that will not
				// happen; the deferred discard removes the spooled file.
				a.deleteIngestRow(r.Context(), ingestedFile.ID, userID)
				writeInternalError(w, "failed to enqueue ingest job", jobErr)
				return
			}
		}

		committed = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(toIngestedFileDTO(ingestedFile, 0))
	}
}

// deleteIngestRow removes an ingest row whose job could not be enqueued. It uses
// its own timeout because the request context may already be cancelled.
func (a *API) deleteIngestRow(ctx context.Context, id, userID uuid.UUID) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := a.db.Queries.DeleteIngestedFile(cleanupCtx, db.DeleteIngestedFileParams{
		ID:     id,
		UserID: userID,
	}); err != nil {
		slog.Error("failed to roll back ingest row",
			slog.Any("error", err),
			slog.String("file_id", id.String()))
	}
}

func (a *API) GetFilesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	filesDB, err := a.db.Queries.GetIngestedFiles(r.Context(), userID)
	if err != nil {
		slog.Error("failed to get files", slog.Any("error", err))
		http.Error(w, "failed to get files", http.StatusInternalServerError)
		return
	}

	// Chunk counts are fetched in one aggregate query instead of one query per
	// file, so a large library stays a constant number of round trips.
	chunkCounts, err := a.db.Queries.GetUserChunkCountsByFile(r.Context(), userID)
	if err != nil {
		slog.Error("failed to get file chunk counts", slog.Any("error", err))
		http.Error(w, "failed to get files", http.StatusInternalServerError)
		return
	}

	countsByFile := make(map[uuid.UUID]int64, len(chunkCounts))
	for _, row := range chunkCounts {
		countsByFile[row.FileID] = row.ChunkCount
	}

	dtos := make([]IngestedFileDTO, 0, len(filesDB))
	for _, f := range filesDB {
		dtos = append(dtos, toIngestedFileDTO(f, countsByFile[f.ID]))
	}

	slog.Info("listed files",
		slog.String("user_id", userID.String()),
		slog.Int("count", len(dtos)))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dtos)
}

func (a *API) FileByIDHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	fileID, category, ok := parseFileRequest(w, r)
	if !ok {
		return
	}
	if category {
		// The category sub-resource is routed to its own handler.
		slog.Warn("invalid path", slog.String("path", r.URL.Path))
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	file, err := a.db.Queries.GetIngestedFileByID(r.Context(), fileID)
	if err != nil {
		slog.Warn("file not found", slog.String("id", fileID.String()), slog.String("path", r.URL.Path))
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	// Another user's file is answered exactly like a missing one: a 403 would tell
	// which ids exist.
	if file.UserID != userID {
		slog.Warn("access to another user's file", slog.String("id", fileID.String()), slog.String("path", r.URL.Path))
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	// A soft deleted row is kept so the delete can be undone, but it is gone as
	// far as clients are concerned.
	if file.DeletedAt.Valid {
		slog.Warn("access to soft deleted file", slog.String("id", fileID.String()), slog.String("path", r.URL.Path))
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toIngestedFileDTO(file, a.fileChunkCount(r.Context(), file.ID, userID)))

	case http.MethodDelete:
		// Soft delete: the row and its chunks are kept, and so is the original
		// upload, so an accidental delete can be undone with one UPDATE. A hard
		// delete cascades into document_chunks, which is how the whole library
		// was destroyed on 2026-10-30 (the 2026-10-30 incident).
		err = a.db.Queries.SoftDeleteIngestedFile(r.Context(), db.SoftDeleteIngestedFileParams{
			ID:     fileID,
			UserID: userID,
		})
		if err != nil {
			slog.Error("failed to delete file from db", slog.Any("error", err))
			http.Error(w, "failed to delete file from db", http.StatusInternalServerError)
			return
		}

		slog.Info("soft deleted ingested file",
			slog.String("file_id", fileID.String()),
			slog.String("local_path", file.LocalPath))

		w.WriteHeader(http.StatusNoContent)
	default:
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) FileCategoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		slog.Warn("method not allowed", slog.String("method", r.Method), slog.String("path", r.URL.Path))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := r.Context().Value(ParsedUserIDKey).(uuid.UUID)
	if !ok {
		slog.Warn("unauthorized access attempt", slog.String("path", r.URL.Path))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	fileID, category, ok := parseFileRequest(w, r)
	if !ok {
		return
	}
	if !category {
		slog.Warn("invalid path", slog.String("path", r.URL.Path))
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	var req struct {
		Category string `json:"category"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !config.IsFileCategory(req.Category) {
		http.Error(w, "category must be one of: "+strings.Join(config.FileCategories, ", "), http.StatusBadRequest)
		return
	}

	// First verify the file exists and belongs to the user
	file, err := a.db.Queries.GetIngestedFileByID(r.Context(), fileID)
	if err != nil {
		slog.Warn("file not found", slog.String("id", fileID.String()), slog.String("path", r.URL.Path))
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	if file.UserID != userID {
		slog.Warn("access to another user's file", slog.String("id", fileID.String()), slog.String("path", r.URL.Path))
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if file.DeletedAt.Valid {
		slog.Warn("category of a soft deleted file", slog.String("id", fileID.String()), slog.String("path", r.URL.Path))
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	err = a.db.Queries.UpdateIngestedFileCategory(r.Context(), db.UpdateIngestedFileCategoryParams{
		ID:       fileID,
		Category: req.Category,
		UserID:   userID,
	})
	if err != nil {
		slog.Error("failed to update file category", slog.Any("error", err))
		http.Error(w, "failed to update file category", http.StatusInternalServerError)
		return
	}

	// Fetch updated file to return
	updatedFile, err := a.db.Queries.GetIngestedFileByID(r.Context(), fileID)
	if err != nil {
		slog.Error("failed to fetch updated file", slog.Any("error", err))
		http.Error(w, "failed to fetch updated file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toIngestedFileDTO(updatedFile, a.fileChunkCount(r.Context(), updatedFile.ID, userID)))
}
