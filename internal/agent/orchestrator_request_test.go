package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
)

// newChatRequest builds a POST /v1/chat/stream request that already carries the
// authenticated user, which is what UserMiddleware would have injected.
func newChatRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/stream", strings.NewReader(body))
	return req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, uuid.New()))
}

func TestHandleStreamChatRejectsOversizedMessage(t *testing.T) {
	// estimateTokens counts four bytes per token, so this is one token over the
	// limit while staying far below the request body cap.
	content := strings.Repeat("a", (maxUserMessageTokens+1)*bytesPerToken)
	body := `{"session_id":"sess-1","content":"` + content + `"}`

	rec := httptest.NewRecorder()
	(&Orchestrator{}).HandleStreamChat(rec, newChatRequest(body))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "at most")
}

func TestValidateUserMessageBoundary(t *testing.T) {
	// estimateTokens charges one token per four bytes, so an exact multiple of
	// bytesPerToken sits precisely on the limit.
	atLimit := strings.Repeat("a", maxUserMessageTokens*bytesPerToken)
	assert.NoError(t, validateUserMessage(atLimit))

	overLimit := strings.Repeat("a", maxUserMessageTokens*bytesPerToken+1)
	err := validateUserMessage(overLimit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most")

	// Cyrillic text costs the same per byte, so the limit does not depend on the
	// alphabet the user writes in.
	cyrillic := strings.Repeat("б", (maxUserMessageTokens+1)*bytesPerToken/2)
	assert.Error(t, validateUserMessage(cyrillic))
}

func TestHandleStreamChatRejectsOversizedBody(t *testing.T) {
	body := `{"session_id":"sess-1","content":"` + strings.Repeat("a", maxChatBodyBytes) + `"}`

	rec := httptest.NewRecorder()
	(&Orchestrator{}).HandleStreamChat(rec, newChatRequest(body))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(t, rec.Body.String(), "request body too large")
}

func TestHandleRegenerateRejectsMalformedBody(t *testing.T) {
	rec := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/regenerate", strings.NewReader(`{"session_id": 7}`))
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, uuid.New()))

	(&Orchestrator{}).HandleRegenerate(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid request body", strings.TrimSpace(rec.Body.String()))
}
