package attachments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
)

// serve runs a request as the given user (the zero UUID means "no user") through the routes.
func (e *env) serve(user uuid.UUID, method, target string, body any) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	e.svc.RegisterRoutes(mux)

	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	if user != uuid.Nil {
		req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, user))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func upload(session, name, content string) map[string]string {
	return map[string]string{"session_id": session, "name": name, "content": content}
}

func TestHTTPAttachListAndDelete(t *testing.T) {
	e := newEnv(t)

	rec := e.serve(e.userID, http.MethodPost, "/chat/attachments", upload("s1", "main.go", "package main\n\nfunc main() {}\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created Added
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.True(t, created.Created)
	assert.Equal(t, "main.go", created.Name)
	assert.Equal(t, 3, created.Lines)
	assert.NotContains(t, rec.Body.String(), "package main", "the answer never carries the content")

	rec = e.serve(e.userID, http.MethodPost, "/chat/attachments", upload("s1", "main.go", "package main\n\nfunc main() {}\n"))
	require.Equal(t, http.StatusOK, rec.Code, "the same file again is not a new file")
	assert.Contains(t, rec.Body.String(), `"unchanged":true`)

	rec = e.serve(e.userID, http.MethodPost, "/chat/attachments", upload("s1", "main.go", "package main\n"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"replaced":true`)

	rec = e.serve(e.userID, http.MethodGet, "/chat/attachments?session_id=s1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var listed []Info
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed, 1)
	assert.Equal(t, created.Info.ID, listed[0].ID)
	assert.Equal(t, 2, listed[0].Version)
	assert.NotContains(t, rec.Body.String(), "package main")

	rec = e.serve(e.userID, http.MethodGet, "/chat/attachments?session_id=another", nil)
	assert.JSONEq(t, `[]`, rec.Body.String(), "an empty list is [], not null")

	rec = e.serve(e.userID, http.MethodDelete, "/chat/attachments/"+created.Info.ID.String(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	rec = e.serve(e.userID, http.MethodDelete, "/chat/attachments/"+created.Info.ID.String(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "twice")
}

func TestHTTPErrors(t *testing.T) {
	e := newEnv(t)
	stranger := e.newUser(t)
	mine := e.serve(e.userID, http.MethodPost, "/chat/attachments", upload("s", "a.txt", "x"))
	require.Equal(t, http.StatusCreated, mine.Code)
	var added Added
	require.NoError(t, json.Unmarshal(mine.Body.Bytes(), &added))

	tests := []struct {
		name   string
		user   uuid.UUID
		method string
		target string
		body   any
		want   int
	}{
		{"no user", uuid.Nil, http.MethodPost, "/chat/attachments", upload("s", "a.txt", "x"), http.StatusUnauthorized},
		{"no user, list", uuid.Nil, http.MethodGet, "/chat/attachments?session_id=s", nil, http.StatusUnauthorized},
		{"no user, delete", uuid.Nil, http.MethodDelete, "/chat/attachments/" + added.Info.ID.String(), nil, http.StatusUnauthorized},
		{"not json", e.userID, http.MethodPost, "/chat/attachments", "not json", http.StatusBadRequest},
		{"empty file", e.userID, http.MethodPost, "/chat/attachments", upload("s", "a.txt", "  "), http.StatusBadRequest},
		{"binary file", e.userID, http.MethodPost, "/chat/attachments", upload("s", "a.bin", "MZ\x00"), http.StatusBadRequest},
		{"no name", e.userID, http.MethodPost, "/chat/attachments", upload("s", "", "x"), http.StatusBadRequest},
		{"no session", e.userID, http.MethodPost, "/chat/attachments", upload("", "a.txt", "x"), http.StatusBadRequest},
		{"too large", e.userID, http.MethodPost, "/chat/attachments", upload("s", "a.txt", strings.Repeat("a", MaxFileBytes+1)), http.StatusRequestEntityTooLarge},
		{"body far too large", e.userID, http.MethodPost, "/chat/attachments", upload("s", "a.txt", strings.Repeat("a", 4<<20)), http.StatusRequestEntityTooLarge},
		{"list needs a session", e.userID, http.MethodGet, "/chat/attachments", nil, http.StatusBadRequest},
		{"id is not a uuid", e.userID, http.MethodDelete, "/chat/attachments/abc", nil, http.StatusBadRequest},
		{"unknown id", e.userID, http.MethodDelete, "/chat/attachments/" + uuid.NewString(), nil, http.StatusNotFound},
		{"someone else's id", stranger, http.MethodDelete, "/chat/attachments/" + added.Info.ID.String(), nil, http.StatusNotFound},
		{"wrong method", e.userID, http.MethodPut, "/chat/attachments", nil, http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.serve(tc.user, tc.method, tc.target, tc.body)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}

	still := e.serve(e.userID, http.MethodGet, "/chat/attachments?session_id=s", nil)
	assert.Contains(t, still.Body.String(), `"a.txt"`, "the refused requests changed nothing, and a stranger could not delete the file")
}

func TestHTTPLimitsAnswerWithUnprocessable(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < MaxSessionFiles; i++ {
		rec := e.serve(e.userID, http.MethodPost, "/chat/attachments", upload("s", fmt.Sprintf("f%02d.txt", i), "x"))
		require.Equal(t, http.StatusCreated, rec.Code)
	}

	rec := e.serve(e.userID, http.MethodPost, "/chat/attachments", upload("s", "one-more.txt", "x"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "20 attached files", "the reason is in the answer")
}

// A client may write every non-ASCII letter as a \uXXXX escape, six bytes for two; a file
// of the maximum size in Cyrillic must still get through.
func TestHTTPAcceptsAnEscapedFileOfTheLimit(t *testing.T) {
	e := newEnv(t)
	text := strings.Repeat("я", MaxFileBytes/2)
	var b strings.Builder
	for _, r := range text {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	body := `{"session_id":"s","name":"bg.txt","content":"` + b.String() + `"}`
	require.Greater(t, len(body), 2*MaxFileBytes)

	rec := e.serve(e.userID, http.MethodPost, "/chat/attachments", body)

	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}
