package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"
)

// uploadDir is where spooled uploads are written. It is a variable so tests can
// point it at a temporary directory.
var uploadDir = "storage"

const (
	// maxUploadBytes caps a single upload request. The body is streamed to disk,
	// so the cap protects storage and job cost rather than memory.
	maxUploadBytes = 100 << 20

	// uploadDetectionBytes is how much of the file is kept for MIME sniffing.
	// mimetype.Detect needs at most the first 3072 bytes.
	uploadDetectionBytes = 3072
)

// clientError carries the status and text that may be shown to a client. The
// wrapped cause is logged but never sent.
type clientError struct {
	status int
	msg    string
	err    error
}

func (e *clientError) Error() string { return e.msg }

func (e *clientError) Unwrap() error { return e.err }

// newClientError builds a client facing error. Server-side failures use
// http.StatusInternalServerError so the handler can log them at error level.
func newClientError(status int, msg string, cause error) *clientError {
	return &clientError{status: status, msg: msg, err: cause}
}

// spooledUpload is a file part that has already been written to uploadDir.
// It owns the file until discard is called.
type spooledUpload struct {
	fileName string
	path     string
	hash     string
	mimeType string
	size     int64
}

// discard removes the spooled file. It is safe to call more than once.
func (u *spooledUpload) discard() {
	if u == nil || u.path == "" {
		return
	}
	if err := os.Remove(u.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("failed to remove upload", slog.Any("error", err), slog.String("path", u.path))
	}
	u.path = ""
}

// spoolUpload streams the "file" part of a multipart request into uploadDir
// while hashing it.
//
// ParseMultipartForm is deliberately not used: it buffers the whole file in
// memory (up to maxMemory) or in the OS temp directory, and the data is then
// copied a second time into storage. Reading the parts directly keeps memory
// flat regardless of file size, and nothing is left behind on failure.
func (a *API) spoolUpload(w http.ResponseWriter, r *http.Request, limit int64) (*spooledUpload, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	reader, err := r.MultipartReader()
	if err != nil {
		return nil, newClientError(http.StatusBadRequest, "invalid multipart form", err)
	}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, newClientError(http.StatusBadRequest, "missing file", http.ErrMissingFile)
		}
		if err != nil {
			return nil, multipartError(err)
		}

		if part.FormName() != "file" {
			// Drain the field so the reader can advance to the next part.
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}

		upload, storeErr := storeUpload(part, part.FileName())
		_ = part.Close()
		return upload, storeErr
	}
}

// multipartError maps a read failure from the multipart reader to a client
// facing error.
func multipartError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return newClientError(http.StatusRequestEntityTooLarge, "file too large", err)
	}
	return newClientError(http.StatusBadRequest, "invalid multipart form", err)
}

// storeUpload copies src to a random name in uploadDir, hashing it on the way,
// and renames the finished file into place. The temporary name means a crash
// never leaves a half written file under a path the database points at.
func storeUpload(src io.Reader, fileName string) (*spooledUpload, error) {
	if fileName == "" {
		return nil, newClientError(http.StatusBadRequest, "missing file", http.ErrMissingFile)
	}

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return nil, newClientError(http.StatusInternalServerError, internalErrorMessage,
			fmt.Errorf("create upload directory: %w", err))
	}

	tempPath := filepath.Join(uploadDir, uuid.NewString()+".part")
	finalPath := filepath.Join(uploadDir, uuid.NewString()+"_"+filepath.Base(fileName))

	file, err := os.Create(tempPath)
	if err != nil {
		return nil, newClientError(http.StatusInternalServerError, internalErrorMessage,
			fmt.Errorf("create upload file: %w", err))
	}

	buffered := bufio.NewReader(src)
	// Peek keeps the bytes in the buffer, so the sniffed prefix is also written
	// to disk by the copy below.
	head, _ := buffered.Peek(uploadDetectionBytes)

	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), buffered)
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(tempPath)
		var tooLarge *http.MaxBytesError
		if errors.As(copyErr, &tooLarge) {
			return nil, newClientError(http.StatusRequestEntityTooLarge, "file too large", copyErr)
		}
		return nil, newClientError(http.StatusInternalServerError, internalErrorMessage,
			fmt.Errorf("write upload: %w", copyErr))
	}

	if written == 0 {
		_ = os.Remove(tempPath)
		return nil, newClientError(http.StatusBadRequest, "file is empty", errors.New("empty upload"))
	}

	if err := os.Rename(tempPath, finalPath); err != nil {
		_ = os.Remove(tempPath)
		return nil, newClientError(http.StatusInternalServerError, internalErrorMessage,
			fmt.Errorf("finalise upload: %w", err))
	}

	return &spooledUpload{
		fileName: fileName,
		path:     finalPath,
		hash:     hex.EncodeToString(hasher.Sum(nil)),
		mimeType: mimetype.Detect(head).String(),
		size:     written,
	}, nil
}
