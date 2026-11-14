package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"
)

// uploadDir is where spooled uploads are written. It is a variable so tests can
// point it at a temporary directory.
var uploadDir = "storage"

// UploadDir is the directory uploads are stored in (relative to the working
// directory); main logs it as an absolute path at start.
func UploadDir() string { return uploadDir }

// removeOriginal deletes the stored upload of a deleted file. Only a file directly
// in uploadDir is removed: local_path comes from the database, and a path that
// points anywhere else (a restored dump from another layout, a hand edit) is left
// alone and logged. A failure is logged, not returned: the file is already gone
// for the user, and what is left is a stray file in storage.
func removeOriginal(ctx context.Context, localPath string) {
	dir, err := filepath.Abs(uploadDir)
	if err != nil {
		slog.ErrorContext(ctx, "failed to resolve the upload directory", slog.Any("error", err))
		return
	}
	path, err := filepath.Abs(localPath)
	if err != nil || filepath.Dir(path) != dir {
		slog.WarnContext(ctx, "the original of a deleted file is not in the upload directory, so it is left alone",
			slog.String("local_path", localPath))
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.ErrorContext(ctx, "failed to remove the original of a deleted file",
			slog.String("local_path", localPath), slog.Any("error", err))
	}
}

const (
	// maxUploadBytes is the default cap on a single upload request (MAX_UPLOAD_MB
	// overrides it through API.WithUploadLimit). The body is streamed to disk, so
	// the cap protects storage and job cost rather than memory.
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
	setReadDeadline(w, uploadReadTimeout)
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

// maxStorageNameRunes bounds the part of the on-disk name that comes from the client.
const maxStorageNameRunes = 100

// storageName is the part of the name a file gets on disk that comes from the name the
// client sent: the last path element, with every character Windows does not allow in a
// file name (and control characters) replaced by "_", no trailing dots or spaces (Windows
// drops them silently), and at most 100 characters with the extension kept. The file's
// real name is stored separately in the database; this one only has to be a valid, unique
// path. Before, a name such as "a:b?.pdf" made the rename fail with a 500.
func storageName(fileName string) string {
	if i := strings.LastIndexAny(fileName, `/\`); i >= 0 {
		fileName = fileName[i+1:]
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, fileName)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return "file"
	}
	if runes := []rune(name); len(runes) > maxStorageNameRunes {
		ext := filepath.Ext(name)
		if n := utf8.RuneCountInString(ext); n > 10 {
			ext = ""
		}
		keep := maxStorageNameRunes - utf8.RuneCountInString(ext)
		name = string(runes[:keep]) + ext
	}
	return name
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
	finalPath := filepath.Join(uploadDir, uuid.NewString()+"_"+storageName(fileName))

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
