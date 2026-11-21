package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/dbtest"
	"go-monolith/internal/taskplan"
)

// GET /task-plan: the newest plan of a conversation, for the app to show.

func taskPlan(t *testing.T, apiService *api.API, userID uuid.UUID, query string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	apiService.RegisterTaskPlanRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/task-plan"+query, nil)
	if userID != uuid.Nil {
		req = withUser(req, userID)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func makePlan(t *testing.T, database *db.Database, userID uuid.UUID, session string) *taskplan.Plan {
	t.Helper()
	plan, err := taskplan.NewStore(database.Queries).Set(context.Background(), userID, session, "свали източниците",
		[]taskplan.StepInput{{Text: "добави линковете", Tool: "add_to_library"}, {Text: "кажи резултата"}})
	require.NoError(t, err)
	return plan
}

func TestTaskPlanIsNullWhenTheConversationHasNone(t *testing.T) {
	apiService, _, userID := setupAPI(t)

	code, body := taskPlan(t, apiService, userID, "?session_id=s1")

	require.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"plan":null}`, body)
}

func TestTaskPlanShowsTheStepsAndNotTheLedger(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	made := makePlan(t, database, userID, "s1")
	_, err := taskplan.NewStore(database.Queries).Record(context.Background(), userID, "s1", "add_to_library", "add_to_library: 2 of 2 queued")
	require.NoError(t, err)

	code, body := taskPlan(t, apiService, userID, "?session_id=s1")

	require.Equal(t, http.StatusOK, code, body)
	var out struct {
		Plan map[string]any `json:"plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, float64(made.ID), out.Plan["id"])
	assert.Equal(t, "свали източниците", out.Plan["goal"])
	assert.Equal(t, "active", out.Plan["status"])
	steps := out.Plan["steps"].([]any)
	require.Len(t, steps, 2)
	assert.Equal(t, "add_to_library", steps[0].(map[string]any)["tool"])
	assert.Equal(t, "pending", steps[0].(map[string]any)["status"])
	assert.NotContains(t, out.Plan, "ledger", "what the tools did is for the server")
	assert.NotContains(t, body, "2 of 2 queued")
}

func TestTaskPlanShowsAFinishedPlanToo(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	store := taskplan.NewStore(database.Queries)
	_, err := store.Set(context.Background(), userID, "s1", "цел", []taskplan.StepInput{{Text: "а", Tool: taskplan.ToolNone}})
	require.NoError(t, err)
	_, _, err = store.Update(context.Background(), userID, "s1", 1, taskplan.StepDone, "")
	require.NoError(t, err)

	_, body := taskPlan(t, apiService, userID, "?session_id=s1")

	assert.Contains(t, body, `"status":"done"`, "a finished plan shows what was done")
}

func TestTaskPlanIsOfThePersonAndTheConversation(t *testing.T) {
	apiService, database, userID := setupAPI(t)
	other, err := database.Queries.CreateUser(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { dbtest.DeleteUser(t, database.Pool, other.ID) })
	makePlan(t, database, userID, "s1")

	_, mine := taskPlan(t, apiService, userID, "?session_id=s1")
	_, theirs := taskPlan(t, apiService, other.ID, "?session_id=s1")
	_, elsewhere := taskPlan(t, apiService, userID, "?session_id=s2")

	assert.Contains(t, mine, "свали източниците")
	assert.JSONEq(t, `{"plan":null}`, theirs)
	assert.JSONEq(t, `{"plan":null}`, elsewhere)
}

func TestTaskPlanAsksForAConversationAndAPerson(t *testing.T) {
	apiService, _, userID := setupAPI(t)

	code, _ := taskPlan(t, apiService, userID, "")
	assert.Equal(t, http.StatusBadRequest, code, "no session_id")
	code, _ = taskPlan(t, apiService, userID, "?session_id="+url.QueryEscape("a\nb"))
	assert.Equal(t, http.StatusBadRequest, code, "a control character")
	code, _ = taskPlan(t, apiService, userID, "?session_id=x")
	assert.Equal(t, http.StatusOK, code)
	code, _ = taskPlan(t, apiService, uuid.Nil, "?session_id=s1")
	assert.Equal(t, http.StatusUnauthorized, code)

	mux := http.NewServeMux()
	apiService.RegisterTaskPlanRoutes(mux)
	req := withUser(httptest.NewRequest(http.MethodPost, "/task-plan?session_id=s1", nil), userID)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "the plan is shown, not changed, here")
}
