package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"go-monolith/metrics"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type key int

const (
	RequestIDKey key = iota
	UserIDKey
	ParsedUserIDKey
	routeRecorderKey
)

// maxRequestIDLen bounds a client supplied X-Request-ID, which is echoed in
// responses and logs.
const maxRequestIDLen = 64

// unmatchedRouteLabel is the metrics path label for requests that no registered
// route handled. A raw URL must never become a label value: every distinct URL
// would create a new time series.
const unmatchedRouteLabel = "other"

// routeRecorder remembers which mux pattern handled a request. ServeMux sets
// Request.Pattern on the request it routes, but each middleware layer hands the
// next one a copy (WithContext), so the outermost layer never sees the value.
type routeRecorder struct {
	pattern string
}

// RecordRoute wraps a ServeMux so that Middleware can label metrics with the
// matched route pattern. Wrap every mux in the chain; the innermost (most
// specific) pattern wins and the catch-all "/" is ignored.
func RecordRoute(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)

		rec, ok := r.Context().Value(routeRecorderKey).(*routeRecorder)
		if ok && rec.pattern == "" && r.Pattern != "" && r.Pattern != "/" {
			rec.pattern = r.Pattern
		}
	})
}

// methodLabel bounds the method label: HTTP accepts any token as a method.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	}
	return "OTHER"
}

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

// polledPaths are asked again and again by a machine or by a tab that polls, so
// their "incoming request" lines would fill the main log: Prometheus scrapes
// /metrics every 15 s, and the "Бот" tab of the client reads /oracle/decisions every
// 10 s while it is open (and the whole journal, page by page, for its counters). Only
// the exact path counts: /oracle/evaluate, the bot's call, is real traffic.
var polledPaths = map[string]bool{
	"/metrics":          true,
	"/oracle/decisions": true,
}

// MiddlewareOption changes how Middleware logs.
type MiddlewareOption func(*middlewareConfig)

type middlewareConfig struct {
	metricsLogger *slog.Logger
}

// WithMetricsLogger sends the "incoming request" line of every request to a polled
// path (polledPaths: /metrics and /oracle/decisions) to l instead of the main
// logger, so the polling does not bury the real traffic.
func WithMetricsLogger(l *slog.Logger) MiddlewareOption {
	return func(c *middlewareConfig) { c.metricsLogger = l }
}

// slowRequest is the time after which a finished request is logged even though it
// succeeded. The chat streams run for tens of seconds by design and are left out. A
// variable so that a test does not have to wait five seconds.
var slowRequest = 5 * time.Second

var streamingPaths = map[string]bool{
	"/chat/stream":     true,
	"/chat/regenerate": true,
}

// logFinished writes the "request finished" line that "incoming request" cannot: the status
// and the time. Only the requests worth searching for get one - those that ended with an
// error status (5xx at WARN, 4xx at INFO) and the slow ones - so the log does not double.
// It carries the request_id of the "incoming request" line.
func logFinished(l *slog.Logger, r *http.Request, status int, elapsed time.Duration) {
	slow := elapsed >= slowRequest && !streamingPaths[r.URL.Path]
	if status < 400 && !slow {
		return
	}
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelWarn
	}
	l.LogAttrs(r.Context(), level, "request finished",
		slog.String("method", r.Method), slog.String("path", r.URL.Path),
		slog.Int("status", status), slog.Int64("duration_ms", elapsed.Milliseconds()), slog.Bool("slow", slow))
}

func Middleware(logger *slog.Logger, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	var cfg middlewareConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := r.Header.Get("X-Request-ID")
			if reqID == "" || len(reqID) > maxRequestIDLen {
				reqID = uuid.NewString()
			}
			w.Header().Set("X-Request-ID", reqID)
			userID := r.Header.Get("X-User-ID")

			route := &routeRecorder{}
			ctx := context.WithValue(r.Context(), RequestIDKey, reqID)
			ctx = context.WithValue(ctx, UserIDKey, userID)
			ctx = context.WithValue(ctx, routeRecorderKey, route)

			reqLogger := logger.With(
				slog.String("request_id", reqID),
				slog.String("user_id", userID),
			)

			accessLogger := reqLogger
			if polledPaths[r.URL.Path] && cfg.metricsLogger != nil {
				accessLogger = cfg.metricsLogger.With(
					slog.String("request_id", reqID),
					slog.String("user_id", userID),
				)
			}
			logged := r.URL.Path != "/healthz"
			if logged {
				accessLogger.Info("incoming request", slog.String("method", r.Method), slog.String("path", r.URL.Path))
			}

			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rw, r.WithContext(ctx))

			elapsed := time.Since(start)
			duration := elapsed.Seconds()

			if logged {
				logFinished(accessLogger, r, rw.status, elapsed)
			}

			pathForMetrics := route.pattern
			if pathForMetrics == "" {
				pathForMetrics = unmatchedRouteLabel
			}
			method := methodLabel(r.Method)

			metrics.QPS.WithLabelValues(method, pathForMetrics, strconv.Itoa(rw.status)).Inc()
			metrics.Latency.WithLabelValues(method, pathForMetrics).Observe(duration)
		})
	}
}

func UserMiddleware(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return newUserMiddleware(poolUserUpserter{pool: pool})
}

// userUpserter makes sure a users row exists before rows referencing it are
// written. It is an interface so the middleware can be exercised without a
// live PostgreSQL instance.
type userUpserter interface {
	UpsertUser(ctx context.Context, id uuid.UUID) error
}

// poolUserUpserter implements userUpserter on top of a pgx pool.
type poolUserUpserter struct {
	pool *pgxpool.Pool
}

func (u poolUserUpserter) UpsertUser(ctx context.Context, id uuid.UUID) error {
	if u.pool == nil {
		return errors.New("user middleware: nil connection pool")
	}
	_, err := u.pool.Exec(ctx, "INSERT INTO users (id) VALUES ($1) ON CONFLICT (id) DO NOTHING;", id)
	return err
}

const (
	// userCacheTTL is how long a confirmed users row is trusted without going
	// back to the database. The row is created by the first request of a user,
	// so re-asserting it on every request only adds a write to the hot path.
	userCacheTTL = 10 * time.Minute
	// maxKnownUsers bounds the cache; entries nearest to expiry are dropped
	// first once the cap is reached.
	maxKnownUsers = 10000
)

// knownUsers is a bounded TTL cache of user ids whose row is known to exist.
// It is safe for concurrent use.
type knownUsers struct {
	mu      sync.Mutex
	expires map[uuid.UUID]time.Time
}

func newKnownUsers() *knownUsers {
	return &knownUsers{expires: make(map[uuid.UUID]time.Time)}
}

// contains reports whether id was recorded and has not expired yet.
func (k *knownUsers) contains(id uuid.UUID, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()

	expiry, ok := k.expires[id]
	if !ok {
		return false
	}
	if !now.Before(expiry) {
		delete(k.expires, id)
		return false
	}
	return true
}

// add records id as known until now+userCacheTTL, evicting expired entries and,
// if that is not enough, the entries closest to expiry.
func (k *knownUsers) add(id uuid.UUID, now time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.expires[id] = now.Add(userCacheTTL)
	if len(k.expires) <= maxKnownUsers {
		return
	}

	for candidate, expiry := range k.expires {
		if !now.Before(expiry) {
			delete(k.expires, candidate)
		}
	}
	for len(k.expires) > maxKnownUsers {
		var oldestID uuid.UUID
		var oldestExpiry time.Time
		first := true
		for candidate, expiry := range k.expires {
			if first || expiry.Before(oldestExpiry) {
				oldestID, oldestExpiry, first = candidate, expiry, false
			}
		}
		delete(k.expires, oldestID)
	}
}

func newUserMiddleware(users userUpserter) func(http.Handler) http.Handler {
	known := newKnownUsers()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userIDStr := r.Header.Get("X-User-ID")
			if userIDStr == "" {
				slog.WarnContext(r.Context(), "missing X-User-ID header", slog.String("path", r.URL.Path))
				http.Error(w, "missing X-User-ID header", http.StatusUnauthorized)
				return
			}

			userID, err := uuid.Parse(userIDStr)
			if err != nil {
				slog.WarnContext(r.Context(), "invalid X-User-ID header", slog.String("header", truncateRunes(userIDStr, maxLoggedHeaderRunes)), slog.String("path", r.URL.Path))
				http.Error(w, "invalid X-User-ID header", http.StatusBadRequest)
				return
			}

			now := time.Now()
			if !known.contains(userID, now) {
				if err := users.UpsertUser(r.Context(), userID); err != nil {
					slog.ErrorContext(r.Context(), "failed to upsert user", slog.Any("error", err), slog.String("user_id", userID.String()))
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				known.add(userID, now)
			}

			ctx := context.WithValue(r.Context(), ParsedUserIDKey, userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
