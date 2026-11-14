package api_test

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
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
)

// serveRules sends a request through the rule routes the way main wires them.
func serveRules(a *api.API, userID uuid.UUID, method, target string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	a.RegisterRuleRoutes(mux)

	req := httptest.NewRequest(method, target, nil)
	if userID != uuid.Nil {
		req = withUser(req, userID)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func saveRule(t *testing.T, database *db.Database, userID uuid.UUID, key, value string) {
	t.Helper()
	saveRuleOfKind(t, database, userID, "rule", key, value)
}

func saveRuleOfKind(t *testing.T, database *db.Database, userID uuid.UUID, kind, key, value string) {
	t.Helper()
	require.NoError(t, database.Queries.SaveGlobalRule(context.Background(), db.SaveGlobalRuleParams{
		UserID: userID, Kind: kind, Key: key, Value: value,
	}))
}

func TestListRules(t *testing.T) {
	a, database, userID := setupAPI(t)

	rec := serveRules(a, userID, http.MethodGet, "/rules")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String(), "no rules is [], not null")

	saveRule(t, database, userID, "language", "Говори на български")
	saveRule(t, database, userID, "tone", "Бъди кратка")

	rec = serveRules(a, userID, http.MethodGet, "/rules")
	require.Equal(t, http.StatusOK, rec.Code)
	var rules []api.RuleDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rules))
	require.Len(t, rules, 2)
	assert.Equal(t, "tone", rules[0].Key, "newest first")
	assert.Equal(t, "Бъди кратка", rules[0].Value)
	assert.True(t, rules[0].Enabled)
	assert.NotZero(t, rules[0].ID)
	assert.Equal(t, "rule", rules[0].Kind)
}

// The notes on how to talk are listed with the rules, marked by their kind, so the
// client can show them apart and delete them the same way (plan item P42). A note
// may share its key with a rule.
func TestListRulesShowsTheKindOfEach(t *testing.T) {
	a, database, userID := setupAPI(t)
	saveRule(t, database, userID, "humour", "Без шеги в работни въпроси")
	saveRuleOfKind(t, database, userID, "style", "humour", "Шегува се — отговаряй с шега")

	rec := serveRules(a, userID, http.MethodGet, "/rules")
	require.Equal(t, http.StatusOK, rec.Code)
	var rules []api.RuleDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rules))
	kinds := map[string]string{}
	for _, r := range rules {
		kinds[r.Kind] = r.Value
	}
	assert.Equal(t, map[string]string{"rule": "Без шеги в работни въпроси", "style": "Шегува се — отговаряй с шега"}, kinds)
}

func TestListRulesOnlyShowsTheCallersRules(t *testing.T) {
	a, database, userID := setupAPI(t)
	other, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	saveRule(t, database, other.ID, "secret", "чужда стойност")
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })

	rec := serveRules(a, userID, http.MethodGet, "/rules")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

func TestDeleteRule(t *testing.T) {
	a, database, userID := setupAPI(t)
	saveRule(t, database, userID, "language", "Говори на български")
	rules, err := database.Queries.ListGlobalRules(context.Background(), userID)
	require.NoError(t, err)
	require.Len(t, rules, 1)

	rec := serveRules(a, userID, http.MethodDelete, fmt.Sprintf("/rules/%d", rules[0].ID))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	left, err := database.Queries.GetGlobalRules(context.Background(), userID)
	require.NoError(t, err)
	assert.Empty(t, left, "a deleted rule is gone from the prompt")

	rec = serveRules(a, userID, http.MethodDelete, fmt.Sprintf("/rules/%d", rules[0].ID))
	assert.Equal(t, http.StatusNotFound, rec.Code, "deleting twice")
}

func TestDeleteRuleOfAnotherUserIsNotFound(t *testing.T) {
	a, database, userID := setupAPI(t)
	other, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })
	saveRule(t, database, other.ID, "theirs", "чужда стойност")
	theirs, err := database.Queries.ListGlobalRules(context.Background(), other.ID)
	require.NoError(t, err)
	require.Len(t, theirs, 1)

	rec := serveRules(a, userID, http.MethodDelete, fmt.Sprintf("/rules/%d", theirs[0].ID))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	still, err := database.Queries.GetGlobalRules(context.Background(), other.ID)
	require.NoError(t, err)
	assert.Len(t, still, 1)
}

func TestRuleRoutesRejectBadRequests(t *testing.T) {
	a, _, userID := setupAPI(t)

	tests := []struct {
		name   string
		user   uuid.UUID
		method string
		target string
		want   int
	}{
		{"not a number", userID, http.MethodDelete, "/rules/abc", http.StatusBadRequest},
		{"zero", userID, http.MethodDelete, "/rules/0", http.StatusBadRequest},
		{"too large for the id", userID, http.MethodDelete, "/rules/99999999999", http.StatusBadRequest},
		{"list needs a user", uuid.Nil, http.MethodGet, "/rules", http.StatusUnauthorized},
		{"delete needs a user", uuid.Nil, http.MethodDelete, "/rules/1", http.StatusUnauthorized},
		{"rules are not created here", userID, http.MethodPost, "/rules", http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRules(a, tc.user, tc.method, tc.target)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}
