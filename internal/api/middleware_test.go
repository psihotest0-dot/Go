package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingUpserter struct {
	calls []uuid.UUID
	err   error
}

func (u *countingUpserter) UpsertUser(ctx context.Context, id uuid.UUID) error {
	if u.err != nil {
		return u.err
	}
	u.calls = append(u.calls, id)
	return nil
}

func TestUserMiddlewareUpsertsOncePerUser(t *testing.T) {
	users := &countingUpserter{}
	userID := uuid.New()

	var seen uuid.UUID
	handler := newUserMiddleware(users)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = r.Context().Value(ParsedUserIDKey).(uuid.UUID)
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
		req.Header.Set("X-User-ID", userID.String())
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, userID, seen, "user id must reach the handler through the context")
	}

	assert.Equal(t, []uuid.UUID{userID}, users.calls, "the users row must be written once, not on every request")
}

func TestUserMiddlewareUpsertsPerDistinctUser(t *testing.T) {
	users := &countingUpserter{}
	handler := newUserMiddleware(users)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	first, second := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{first, second, first} {
		req := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
		req.Header.Set("X-User-ID", id.String())
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	assert.Equal(t, []uuid.UUID{first, second}, users.calls)
}

func TestUserMiddlewareRejectsBadHeaders(t *testing.T) {
	handler := newUserMiddleware(&countingUpserter{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached")
	}))

	tests := []struct {
		name   string
		header string
		status int
	}{
		{name: "missing", header: "", status: http.StatusUnauthorized},
		{name: "not a uuid", header: "not-a-uuid", status: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
			if tt.header != "" {
				req.Header.Set("X-User-ID", tt.header)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			assert.Equal(t, tt.status, rec.Code)
		})
	}
}

func TestUserMiddlewareReportsUpsertFailure(t *testing.T) {
	users := &countingUpserter{err: errors.New(`pq: relation "users" does not exist`)}
	handler := newUserMiddleware(users)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
	req.Header.Set("X-User-ID", uuid.New().String())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "users", "database details must not be echoed")
}

func TestKnownUsersExpire(t *testing.T) {
	cache := newKnownUsers()
	id := uuid.New()
	now := time.Now()

	assert.False(t, cache.contains(id, now))

	cache.add(id, now)
	assert.True(t, cache.contains(id, now.Add(time.Minute)))

	expired := now.Add(userCacheTTL)
	assert.False(t, cache.contains(id, expired), "entries older than the TTL must be dropped")
}

func TestKnownUsersStaysBounded(t *testing.T) {
	cache := newKnownUsers()
	now := time.Now()

	for i := 0; i < maxKnownUsers+50; i++ {
		cache.add(uuid.New(), now)
	}

	assert.LessOrEqual(t, len(cache.expires), maxKnownUsers)
}

func TestWriteInternalErrorHidesDetails(t *testing.T) {
	rec := httptest.NewRecorder()

	writeInternalError(rec, httptest.NewRequest(http.MethodPost, "/messages", nil), "failed to create message", errors.New("pq: duplicate key value violates unique constraint"))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	body := strings.TrimSpace(rec.Body.String())
	assert.Equal(t, internalErrorMessage, body)
	assert.NotContains(t, body, "duplicate key")
}

func TestWriteInvalidBodyHidesDecodeDetails(t *testing.T) {
	rec := httptest.NewRecorder()

	writeInvalidBody(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), errors.New("json: cannot unmarshal string into Go struct field ChatRequest.content"))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	body := strings.TrimSpace(rec.Body.String())
	assert.Equal(t, "invalid request body", body)
	assert.NotContains(t, body, "ChatRequest")
}
