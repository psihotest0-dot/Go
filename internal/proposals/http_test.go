package proposals

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
)

// serve runs a request as the given user through the routes of the service.
func (e *env) serve(t *testing.T, userID uuid.UUID, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	e.svc.RegisterRoutes(mux)

	req := httptest.NewRequest(method, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), api.ParsedUserIDKey, userID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHTTPListApproveReject(t *testing.T) {
	e := newEnv(t, nil)
	rule, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "language", "Говори на български", []string{"web_fetch"})
	require.NoError(t, err)
	memory, err := e.svc.ProposeMemory(e.ctx, e.userID, "s", "Обича джаз", 6, nil)
	require.NoError(t, err)

	rec := e.serve(t, e.userID, http.MethodGet, "/proposals")
	require.Equal(t, http.StatusOK, rec.Code)
	var listed []Proposal
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed, 2)
	assert.Equal(t, rule.ID, listed[0].ID)
	assert.Equal(t, []string{"web_fetch"}, listed[0].ExternalSources)
	assert.Equal(t, "language", listed[0].Key)
	assert.Equal(t, "Обича джаз", listed[1].Content)

	rec = e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/approve", rule.ID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"Говори на български"}, e.rules(t, e.userID))

	rec = e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/reject", memory.ID))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, e.memories(t, e.userID))

	rec = e.serve(t, e.userID, http.MethodGet, "/proposals")
	assert.JSONEq(t, `[]`, rec.Body.String(), "an empty list is [], not null")
}

func TestHTTPErrors(t *testing.T) {
	e := newEnv(t, nil)
	stranger := e.newUser(t)
	p, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "k", "v", nil)
	require.NoError(t, err)

	tests := []struct {
		name   string
		user   uuid.UUID
		method string
		target string
		want   int
	}{
		{"unknown id", e.userID, http.MethodPost, "/proposals/999999999/approve", http.StatusNotFound},
		{"someone else's id", stranger, http.MethodPost, fmt.Sprintf("/proposals/%d/approve", p.ID), http.StatusNotFound},
		{"not a number", e.userID, http.MethodPost, "/proposals/abc/approve", http.StatusBadRequest},
		{"zero", e.userID, http.MethodPost, "/proposals/0/reject", http.StatusBadRequest},
		{"negative", e.userID, http.MethodPost, "/proposals/-3/reject", http.StatusBadRequest},
		{"approving needs POST", e.userID, http.MethodGet, fmt.Sprintf("/proposals/%d/approve", p.ID), http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.serve(t, tc.user, tc.method, tc.target)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
	assert.Empty(t, e.rules(t, e.userID), "none of the failed calls approved anything")

	// A second decision is a conflict, not a second write.
	require.Equal(t, http.StatusOK, e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/approve", p.ID)).Code)
	rec := e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/approve", p.ID))
	assert.Equal(t, http.StatusConflict, rec.Code)
	rec = e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/reject", p.ID))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestHTTPRequiresAUser(t *testing.T) {
	e := newEnv(t, nil)
	mux := http.NewServeMux()
	e.svc.RegisterRoutes(mux)

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/proposals"},
		{http.MethodPost, "/proposals/1/approve"},
		{http.MethodPost, "/proposals/1/reject"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, tc.target)
	}
}

func TestHTTPRuleLimitIsUnprocessable(t *testing.T) {
	e := newEnv(t, nil)
	p, err := e.svc.ProposeRule(e.ctx, e.userID, "s", "late", "v", nil)
	require.NoError(t, err)
	for i := 0; i < MaxActiveRules; i++ {
		_, err := e.database.Pool.Exec(e.ctx,
			"INSERT INTO global_rules (user_id, key, value) VALUES ($1, $2, 'x')", e.userID, fmt.Sprintf("r%d", i))
		require.NoError(t, err)
	}

	rec := e.serve(t, e.userID, http.MethodPost, fmt.Sprintf("/proposals/%d/approve", p.ID))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "409 is reserved for a proposal that was decided already")
	assert.Contains(t, rec.Body.String(), "too many global rules")

	pending, err := e.svc.Pending(e.ctx, e.userID)
	require.NoError(t, err)
	assert.Len(t, pending, 1, "the proposal stays open")
}
