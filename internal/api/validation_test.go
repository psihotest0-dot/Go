package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMessageInput(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		role      string
		content   string
		wantErr   string
	}{
		{name: "valid user turn", sessionID: "sess", role: "user", content: "Здравей"},
		{name: "valid assistant turn", sessionID: "sess", role: "assistant", content: "Здравей"},
		{name: "valid system turn", sessionID: "sess", role: "system", content: "rules"},
		{name: "missing session", sessionID: "", role: "user", content: "hi", wantErr: "session_id is required"},
		{name: "blank session", sessionID: "   ", role: "user", content: "hi", wantErr: "session_id is required"},
		{
			name:      "session too long",
			sessionID: strings.Repeat("s", maxSessionIDChars+1),
			role:      "user",
			content:   "hi",
			wantErr:   "session_id must be at most",
		},
		{name: "unknown role", sessionID: "sess", role: "admin", content: "hi", wantErr: "role must be one of"},
		{name: "empty role", sessionID: "sess", role: "", content: "hi", wantErr: "role must be one of"},
		{name: "case sensitive role", sessionID: "sess", role: "User", content: "hi", wantErr: "role must be one of"},
		{name: "empty content", sessionID: "sess", role: "user", content: "", wantErr: "content is required"},
		{name: "blank content", sessionID: "sess", role: "user", content: " \n\t", wantErr: "content is required"},
		{
			name:      "content too long",
			sessionID: "sess",
			role:      "user",
			content:   strings.Repeat("a", maxContentChars+1),
			wantErr:   "content must be at most",
		},
		{
			name:      "content at limit",
			sessionID: "sess",
			role:      "user",
			content:   strings.Repeat("a", maxContentChars),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMessageInput(tt.sessionID, tt.role, tt.content)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateMemoryInput(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		status     string
		importance int32
		wantErr    string
	}{
		{name: "valid candidate", content: "Живея в София", status: "candidate", importance: 5},
		{name: "valid active", content: "Живея в София", status: "active", importance: 1},
		{name: "superseded is gone", content: "Живея в София", status: "superseded", importance: 10, wantErr: "status must be one of"},
		{name: "empty content", content: "  ", status: "candidate", importance: 5, wantErr: "content is required"},
		{name: "unknown status", content: "x", status: "archived", importance: 5, wantErr: "status must be one of"},
		{name: "empty status", content: "x", status: "", importance: 5, wantErr: "status must be one of"},
		{name: "importance too low", content: "x", status: "active", importance: 0, wantErr: "importance must be between"},
		{name: "importance negative", content: "x", status: "active", importance: -3, wantErr: "importance must be between"},
		{name: "importance too high", content: "x", status: "active", importance: 11, wantErr: "importance must be between"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMemoryInput(tt.content, tt.status, tt.importance)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestDecodeJSONRejectsOversizedBody(t *testing.T) {
	body := `{"session_id":"` + strings.Repeat("a", maxJSONBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	var dst struct {
		SessionID string `json:"session_id"`
	}
	require.False(t, decodeJSON(rec, req, &dst))

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(t, rec.Body.String(), "request body too large")
}

func TestDecodeJSONRejectsMalformedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"session_id": 42}`))
	rec := httptest.NewRecorder()

	var dst struct {
		SessionID string `json:"session_id"`
	}
	require.False(t, decodeJSON(rec, req, &dst))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid request body", strings.TrimSpace(rec.Body.String()))
}

// TestCreateMessageHandlerRejectsInvalidRole exercises validation through the
// handler. The API service deliberately has no database handle: an invalid
// payload must be refused before anything touches storage.
func TestCreateMessageHandlerRejectsInvalidRole(t *testing.T) {
	service := New(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"session_id":"sess-1","role":"root","content":"hi"}`))
	req = req.WithContext(context.WithValue(req.Context(), ParsedUserIDKey, uuid.New()))
	rec := httptest.NewRecorder()

	service.CreateMessageHandler(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "role must be one of")
}

func TestCreateMessageHandlerRejectsEmptyContent(t *testing.T) {
	service := New(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"session_id":"sess-1","role":"user","content":"   "}`))
	req = req.WithContext(context.WithValue(req.Context(), ParsedUserIDKey, uuid.New()))
	rec := httptest.NewRecorder()

	service.CreateMessageHandler(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "content is required")
}

func TestCreateMemoryHandlerRejectsEmptyContent(t *testing.T) {
	service := New(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/memories",
		strings.NewReader(`{"content":"   ","importance":5}`))
	req = req.WithContext(context.WithValue(req.Context(), ParsedUserIDKey, uuid.New()))
	rec := httptest.NewRecorder()

	service.CreateMemoryHandler(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "content is required")
}

func TestCreateMemoryHandlerRejectsInvalidImportance(t *testing.T) {
	service := New(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/memories",
		strings.NewReader(`{"content":"Живея в София","importance":42}`))
	req = req.WithContext(context.WithValue(req.Context(), ParsedUserIDKey, uuid.New()))
	rec := httptest.NewRecorder()

	service.CreateMemoryHandler(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "importance must be between")
}

func TestValidateImportanceRanges(t *testing.T) {
	assert.NoError(t, validateImportance(minImportance))
	assert.NoError(t, validateImportance(maxImportance))
	assert.Error(t, validateImportance(minImportance-1))
	assert.Error(t, validateImportance(maxImportance+1))

	// The error text must tell the caller what the accepted range is.
	assert.Contains(t, validateImportance(maxImportance+1).Error(), "1 and 10")
}
