package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddlewareRequestID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name     string
		header   string
		wantSame bool
	}{
		{name: "client id is kept", header: "client-req-1", wantSame: true},
		{name: "missing id is generated"},
		{name: "oversized id is replaced", header: strings.Repeat("x", maxRequestIDLen+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var inContext string
			handler := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inContext, _ = r.Context().Value(RequestIDKey).(string)
			}))

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tc.header != "" {
				req.Header.Set("X-Request-ID", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-ID")
			require.NotEmpty(t, got)
			assert.Equal(t, got, inContext, "the id in the context must be the one sent back")
			if tc.wantSame {
				assert.Equal(t, tc.header, got)
				return
			}
			_, err := uuid.Parse(got)
			assert.NoError(t, err, "a generated id must be a UUID, got %q", got)
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	cyrillic := strings.Repeat("ж", 700)

	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "short text is unchanged", in: "кратък текст", max: 600, want: "кратък текст"},
		{name: "exactly at the limit is unchanged", in: strings.Repeat("a", 600), max: 600, want: strings.Repeat("a", 600)},
		{name: "ascii is cut", in: strings.Repeat("a", 601), max: 600, want: strings.Repeat("a", 600) + "..."},
		{name: "cyrillic is cut by runes, not bytes", in: cyrillic, max: 600, want: strings.Repeat("ж", 600) + "..."},
		{name: "empty", in: "", max: 5, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateRunes(tc.in, tc.max)
			assert.Equal(t, tc.want, got)
			assert.True(t, utf8.ValidString(got), "result must stay valid UTF-8")
		})
	}
}
