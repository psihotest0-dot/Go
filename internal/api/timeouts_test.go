package api

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startServer runs handler on the application's real server settings.
func startServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := NewHTTPServer(handler)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + listener.Addr().String()
}

func TestNewHTTPServerHasNoWholeRequestReadTimeout(t *testing.T) {
	srv := NewHTTPServer(http.NotFoundHandler())

	// ReadTimeout spans the request body: with it a big upload over a slow disk
	// or link was cut off after 30 s.
	assert.Zero(t, srv.ReadTimeout)
	assert.Positive(t, srv.ReadHeaderTimeout, "slow-header clients must still be dropped")
}

// slowMultipart streams a multipart upload in chunks with pauses, so that the
// whole body takes about chunks*pause to arrive.
func slowMultipart(chunks int, pause time.Duration) (io.Reader, string, int) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	chunk := bytes.Repeat([]byte("a"), 64<<10)

	go func() {
		part, err := writer.CreateFormFile("file", "slow.txt")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		for i := 0; i < chunks; i++ {
			if _, err := part.Write(chunk); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			time.Sleep(pause)
		}
		_ = pw.CloseWithError(writer.Close())
	}()

	return pr, writer.FormDataContentType(), chunks * len(chunk)
}

// An upload that takes longer than the JSON deadline must still be accepted.
func TestSlowUploadIsNotCutOff(t *testing.T) {
	withTempUploadDir(t)
	prevJSON, prevUpload := jsonReadTimeout, uploadReadTimeout
	jsonReadTimeout, uploadReadTimeout = 200*time.Millisecond, 5*time.Second
	t.Cleanup(func() { jsonReadTimeout, uploadReadTimeout = prevJSON, prevUpload })

	var stored atomic.Int64
	url := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upload, err := (&API{}).spoolUpload(w, r, maxUploadBytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer upload.discard()
		stored.Store(upload.size)
		w.WriteHeader(http.StatusCreated)
	}))

	body, contentType, wantSize := slowMultipart(8, 80*time.Millisecond) // ~640 ms > 200 ms
	req, err := http.NewRequest(http.MethodPost, url, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, int64(wantSize), stored.Load())
}

// The deadline is what bounds a stalled JSON body now that ReadTimeout is gone.
func TestSlowJSONBodyIsCutOff(t *testing.T) {
	prev := jsonReadTimeout
	jsonReadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { jsonReadTimeout = prev })

	var decoded atomic.Bool
	finished := make(chan struct{})
	url := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		var v map[string]string
		decoded.Store(decodeJSON(w, r, &v))
	}))

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"key":"`))
		time.Sleep(600 * time.Millisecond) // longer than the deadline
		_, _ = pw.Write([]byte(`value"}`))
		_ = pw.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, url, pr)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never returned")
	}
	assert.False(t, decoded.Load(), "a body that stalls past the deadline must not decode")
}

func TestCreateFileHandlerRejectsUploadOverConfiguredLimit(t *testing.T) {
	withTempUploadDir(t)

	body, contentType := multipartBody(t, "big.bin", bytes.Repeat([]byte("x"), 4096))
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Content-Type", contentType)
	req = req.WithContext(context.WithValue(req.Context(), ParsedUserIDKey, uuid.New()))
	rec := httptest.NewRecorder()

	(&API{}).WithUploadLimit(1024).CreateFileHandler(nil)(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "file too large", strings.TrimSpace(rec.Body.String()))
}

func TestUploadLimitBytes(t *testing.T) {
	assert.Equal(t, int64(maxUploadBytes), (&API{}).uploadLimitBytes(), "unset means the default")
	assert.Equal(t, int64(5<<20), (&API{}).WithUploadLimit(5<<20).uploadLimitBytes())
}
