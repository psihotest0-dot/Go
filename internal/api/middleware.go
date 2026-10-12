package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go-monolith/metrics"
)

type key int

const (
	RequestIDKey key = iota
	UserIDKey
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
			reqLogger.Info("incoming request", slog.String("method", r.Method), slog.String("path", r.URL.Path))

			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rw, r.WithContext(ctx))

			duration := time.Since(start).Seconds()

			metrics.QPS.WithLabelValues(r.Method, r.URL.Path, strconv.Itoa(rw.status)).Inc()
			metrics.Latency.WithLabelValues(r.Method, r.URL.Path).Observe(duration)
		})
	}
}
