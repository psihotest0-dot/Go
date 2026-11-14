package userfiles

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
)

// serve runs a request as the user through the routes of the store.
func serve(t *testing.T, s *Store, user uuid.UUID, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	req := httptest.NewRequest(method, target, nil)
	if user != uuid.Nil {
		req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, user))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestStoreUsageTextAndDelete(t *testing.T) {
	s, dir := newStore(t, Limits{})
	_, err := s.Create(alice, "a.txt", "първи")
	require.NoError(t, err)
	_, err = s.Replace(alice, "a.txt", "втори")
	require.NoError(t, err)
	_, err = s.Create(alice, "b.md", "x")
	require.NoError(t, err)

	files, bytes, err := s.Usage(alice)
	require.NoError(t, err)
	assert.Equal(t, 2, files)
	assert.EqualValues(t, len("втори")+len("първи")+1, bytes, "the previous version takes room too")

	text, info, err := s.Text(alice, "a.txt")
	require.NoError(t, err)
	assert.Equal(t, "втори", text)
	assert.Equal(t, "a.txt", info.Name)
	_, _, err = s.Text(alice, "missing.txt")
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = s.Text(bob, "a.txt")
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, os.WriteFile(filepath.Join(userDir(dir, alice), "bom.txt"), []byte("\xef\xbb\xbfhello\r\nworld"), 0o600))
	text, _, err = s.Text(alice, "bom.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello\r\nworld", text, "the whole text, only the byte order mark is dropped")
	require.NoError(t, os.WriteFile(filepath.Join(userDir(dir, alice), "bad.txt"), []byte("ab\xff"), 0o600))
	var invalid *InvalidError
	_, _, err = s.Text(alice, "bad.txt")
	require.ErrorAs(t, err, &invalid)

	require.NoError(t, s.Delete(alice, "a.txt"))
	_, err = s.Stat(alice, "a.txt")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Previous(alice, "a.txt")
	require.ErrorIs(t, err, ErrNotFound, "the previous version goes with the file")
	require.ErrorIs(t, s.Delete(alice, "a.txt"), ErrNotFound)
	require.ErrorIs(t, s.Delete(bob, "b.md"), ErrNotFound, "another user's file")
	_, err = s.Stat(alice, "b.md")
	require.NoError(t, err)
	require.ErrorAs(t, s.Delete(alice, "../b.md"), &invalid)

	require.NoError(t, os.Mkdir(filepath.Join(userDir(dir, alice), "folder.txt"), 0o700))
	require.ErrorIs(t, s.Delete(alice, "folder.txt"), ErrNotFound, "a folder is not a file")
	_, err = os.Stat(filepath.Join(userDir(dir, alice), "folder.txt"))
	require.NoError(t, err)
}

func TestHTTPListGetAndDelete(t *testing.T) {
	s, _ := newStore(t, Limits{})

	rec := serve(t, s, alice, http.MethodGet, "/workspace/files")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"files":[]`, "an empty folder is [], not null")
	assert.Contains(t, rec.Body.String(), `"max_files":200`)

	_, err := s.Create(alice, "списък за пазаруване.txt", "мляко")
	require.NoError(t, err)
	_, err = s.Replace(alice, "списък за пазаруване.txt", "мляко и хляб")
	require.NoError(t, err)

	rec = serve(t, s, alice, http.MethodGet, "/workspace/files")
	require.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		Files []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"files"`
		TotalBytes    int64 `json:"total_bytes"`
		MaxFileBytes  int   `json:"max_file_bytes"`
		MaxTotalBytes int64 `json:"max_total_bytes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Files, 1)
	assert.Equal(t, "списък за пазаруване.txt", list.Files[0].Name)
	assert.EqualValues(t, len("мляко и хляб"), list.Files[0].Size)
	assert.EqualValues(t, len("мляко и хляб")+len("мляко"), list.TotalBytes)
	assert.Equal(t, DefaultMaxFileBytes, list.MaxFileBytes)
	assert.EqualValues(t, DefaultMaxTotalBytes, list.MaxTotalBytes)

	// A name with spaces and Cyrillic travels percent-encoded in the path.
	target := "/workspace/files/%D1%81%D0%BF%D0%B8%D1%81%D1%8A%D0%BA%20%D0%B7%D0%B0%20%D0%BF%D0%B0%D0%B7%D0%B0%D1%80%D1%83%D0%B2%D0%B0%D0%BD%D0%B5.txt"
	rec = serve(t, s, alice, http.MethodGet, target)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var file fileResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &file))
	assert.Equal(t, "мляко и хляб", file.Content)
	assert.Equal(t, "current", file.Version)
	assert.NotNil(t, file.Modified)

	rec = serve(t, s, alice, http.MethodGet, target+"?version=previous")
	require.Equal(t, http.StatusOK, rec.Code)
	var previous fileResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &previous))
	assert.Equal(t, "мляко", previous.Content)
	assert.Equal(t, "previous", previous.Version)
	assert.Nil(t, previous.Modified)

	rec = serve(t, s, alice, http.MethodDelete, target)
	require.Equal(t, http.StatusNoContent, rec.Code)
	rec = serve(t, s, alice, http.MethodGet, target)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = serve(t, s, alice, http.MethodDelete, target)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTPRefusals(t *testing.T) {
	s, _ := newStore(t, Limits{})
	_, err := s.Create(alice, "a.txt", "mine")
	require.NoError(t, err)

	tests := []struct {
		name   string
		user   uuid.UUID
		method string
		target string
		want   int
	}{
		{"no user, list", uuid.Nil, http.MethodGet, "/workspace/files", http.StatusUnauthorized},
		{"no user, read", uuid.Nil, http.MethodGet, "/workspace/files/a.txt", http.StatusUnauthorized},
		{"no user, delete", uuid.Nil, http.MethodDelete, "/workspace/files/a.txt", http.StatusUnauthorized},
		{"another user's file", bob, http.MethodGet, "/workspace/files/a.txt", http.StatusNotFound},
		{"another user's delete", bob, http.MethodDelete, "/workspace/files/a.txt", http.StatusNotFound},
		{"no previous version", alice, http.MethodGet, "/workspace/files/a.txt?version=previous", http.StatusNotFound},
		{"a script", alice, http.MethodGet, "/workspace/files/run.ps1", http.StatusBadRequest},
		{"a stream", alice, http.MethodGet, "/workspace/files/a.txt:s", http.StatusBadRequest},
		{"a way out", alice, http.MethodGet, "/workspace/files/..%2Fa.txt", http.StatusBadRequest},
		{"a way out, delete", alice, http.MethodDelete, "/workspace/files/..%2Fa.txt", http.StatusBadRequest},
		{"a post", alice, http.MethodPost, "/workspace/files", http.StatusMethodNotAllowed},
		{"a put", alice, http.MethodPut, "/workspace/files/a.txt", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, s, tt.user, tt.method, tt.target)
			assert.Equal(t, tt.want, rec.Code, rec.Body.String())
		})
	}

	_, err = s.Stat(alice, "a.txt")
	require.NoError(t, err, "none of the refusals removed the file")
}
