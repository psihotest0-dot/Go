package api

import (
	"context"
	"log/slog"
	"net/http"
)

type key int

const (
	RequestIDKey key = iota
	UserIDKey
)

func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := r.Header.Get("X-Request-ID")
			if reqID == "" {
				reqID = "unknown"
			}
			userID := r.Header.Get("X-User-ID")

			ctx := context.WithValue(r.Context(), RequestIDKey, reqID)
			ctx = context.WithValue(ctx, UserIDKey, userID)

			reqLogger := logger.With(
				slog.String("request_id", reqID),
				slog.String("user_id", userID),
			)
			reqLogger.Info("incoming request", slog.String("method", r.Method), slog.String("path", r.URL.Path))

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
