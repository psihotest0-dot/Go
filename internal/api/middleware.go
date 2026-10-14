package api

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go-monolith/metrics"
)

type key int

const (
	RequestIDKey key = iota
	UserIDKey
	ParsedUserIDKey
)

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func RecoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("panic recovered", slog.Any("error", err), slog.String("stacktrace", string(debug.Stack())))
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
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

			if r.URL.Path != "/healthz" {
				reqLogger.Info("incoming request", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			}

			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rw, r.WithContext(ctx))

			duration := time.Since(start).Seconds()

			metrics.QPS.WithLabelValues(r.Method, r.URL.Path, strconv.Itoa(rw.status)).Inc()
			metrics.Latency.WithLabelValues(r.Method, r.URL.Path).Observe(duration)
		})
	}
}

func UserMiddleware(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userIDStr := r.Header.Get("X-User-ID")
			if userIDStr == "" {
					slog.Warn("missing X-User-ID header", slog.String("path", r.URL.Path))
				http.Error(w, "missing X-User-ID header", http.StatusUnauthorized)
				return
			}

			userID, err := uuid.Parse(userIDStr)
			if err != nil {
					slog.Warn("invalid X-User-ID header", slog.String("header", userIDStr), slog.String("path", r.URL.Path))
				http.Error(w, "invalid X-User-ID header", http.StatusBadRequest)
				return
			}

			_, err = pool.Exec(r.Context(), "INSERT INTO users (id) VALUES ($1) ON CONFLICT (id) DO NOTHING;", userID)
			if err != nil {
				slog.Error("failed to upsert user", slog.Any("error", err))
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			ctx := context.WithValue(r.Context(), ParsedUserIDKey, userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
