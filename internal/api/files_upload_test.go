package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTempUploadDir points uploadDir at a temporary directory for the duration
// of the test, so no file ever lands in the repository.
func withTempUploadDir(t *testing.T) {
	t.Helper()

	previous := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = previous })
}

// multipartBody builds a request body the way a browser would send it.
func multipartBody(t *testing.T, fileName string, content []byte) (io.Reader, string) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	if fileName != "" {
		part, err := writer.CreateFormFile("file", fileName)
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.WriteField("note", "ignored field"))
	require.NoError(t, writer.Close())

	return &buf, writer.FormDataContentType()
}

func TestSpoolUploadStoresHashedFile(t *testing.T) {
	withTempUploadDir(t)

	content := []byte("Здравей, това е тестов файл за upload.")
	body, contentType := multipartBody(t, "test.txt", content)
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()

	upload, err := (&API{}).spoolUpload(rec, req, maxUploadBytes)
	require.NoError(t, err)
	defer upload.discard()

	wantHash := sha256.Sum256(content)
	assert.Equal(t, hex.EncodeToString(wantHash[:]), upload.hash)
	assert.Equal(t, int64(len(content)), upload.size)
	assert.Equal(t, "test.txt", upload.fileName)
	assert.Contains(t, upload.mimeType, "text/plain")

	stored, readErr := os.ReadFile(upload.path)
	require.NoError(t, readErr)
	assert.Equal(t, content, stored, "the stored bytes must match the upload")

	assert.Equal(t, uploadDir, filepath.Dir(upload.path), "the file must live in the upload directory")
	assert.False(t, strings.HasSuffix(upload.path, ".part"), "the temporary name must be renamed away")
}

func TestSpoolUploadDiscardRemovesFile(t *testing.T) {
	withTempUploadDir(t)

	body, contentType := multipartBody(t, "test.txt", []byte("payload"))
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Content-Type", contentType)

	upload, err := (&API{}).spoolUpload(httptest.NewRecorder(), req, maxUploadBytes)
	require.NoError(t, err)

	storedPath := upload.path
	upload.discard()

	_, statErr := os.Stat(storedPath)
	assert.True(t, os.IsNotExist(statErr), "discard must delete the spooled file")

	// A second discard must not fail or panic.
	upload.discard()
}

func TestSpoolUploadRejectsTooLargeBody(t *testing.T) {
	withTempUploadDir(t)

	body, contentType := multipartBody(t, "big.bin", bytes.Repeat([]byte("x"), 4096))
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Content-Type", contentType)

	_, err := (&API{}).spoolUpload(httptest.NewRecorder(), req, 512)
	require.Error(t, err)

	var clientErr *clientError
	require.ErrorAs(t, err, &clientErr)
	assert.Equal(t, http.StatusRequestEntityTooLarge, clientErr.status)

	entries, readErr := os.ReadDir(uploadDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "a rejected upload must not leave files behind")
}

func TestSpoolUploadRejectsMissingFilePart(t *testing.T) {
	withTempUploadDir(t)

	body, contentType := multipartBody(t, "", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Content-Type", contentType)

	_, err := (&API{}).spoolUpload(httptest.NewRecorder(), req, maxUploadBytes)
	require.Error(t, err)

	var clientErr *clientError
	require.ErrorAs(t, err, &clientErr)
	assert.Equal(t, http.StatusBadRequest, clientErr.status)
	assert.Equal(t, "missing file", clientErr.msg)
}

func TestSpoolUploadRejectsNonMultipartBody(t *testing.T) {
	withTempUploadDir(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader(`{"file":"nope"}`))
	req.Header.Set("Content-Type", "application/json")

	_, err := (&API{}).spoolUpload(httptest.NewRecorder(), req, maxUploadBytes)
	require.Error(t, err)

	var clientErr *clientError
	require.ErrorAs(t, err, &clientErr)
	assert.Equal(t, http.StatusBadRequest, clientErr.status)
	assert.Equal(t, "invalid multipart form", clientErr.msg)
}

func TestStoreUploadRejectsEmptyFile(t *testing.T) {
	withTempUploadDir(t)

	_, err := storeUpload(strings.NewReader(""), "empty.txt")
	require.Error(t, err)

	var clientErr *clientError
	require.ErrorAs(t, err, &clientErr)
	assert.Equal(t, http.StatusBadRequest, clientErr.status)
	assert.Equal(t, "file is empty", clientErr.msg)

	entries, readErr := os.ReadDir(uploadDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}
