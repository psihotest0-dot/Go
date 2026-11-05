package oracle

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"go-monolith/internal/api"
	"go-monolith/metrics"
)

// MinTokenLength is the shortest ORACLE_TOKEN that turns the route on. A token is the
// only thing between the network and the model, and the route is reachable from the
// containers on the machine, so a short one is refused instead of trusted.
const MinTokenLength = 32

// TokenUsable reports whether token is long enough to serve the Oracle route with.
func TokenUsable(token string) bool { return len(token) >= MinTokenLength }

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// Handler serves the Oracle routes.
type Handler struct {
	svc   *Service
	store Store
	// tokenDigest is the SHA-256 of the token: comparing digests takes the same time
	// whatever the length of what the caller sent.
	tokenDigest [sha256.Size]byte
}

// NewHandler returns the handlers. token is ORACLE_TOKEN.
func NewHandler(svc *Service, store Store, token string) *Handler {
	return &Handler{svc: svc, store: store, tokenDigest: sha256.Sum256([]byte(token))}
}

// RegisterBotRoutes adds the routes the trading bot calls to mux. They sit outside the
// user check (the caller is not a user of the chat) and carry their own token check.
func (h *Handler) RegisterBotRoutes(mux *http.ServeMux) {
	mux.Handle("/oracle/evaluate", h.bearer(http.HandlerFunc(h.evaluate)))
	mux.Handle("/oracle/ping", h.bearer(http.HandlerFunc(h.ping)))
}

// RegisterReadRoutes adds the journal to mux, which sits behind the user check.
func (h *Handler) RegisterReadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/oracle/decisions", h.decisions)
}

// bearer lets a request through only with "Authorization: Bearer <token>".
func (h *Handler) bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		digest := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(digest[:], h.tokenDigest[:]) != 1 {
			metrics.OracleRequests.WithLabelValues("unauthorized").Inc()
			// The token itself is never logged, nor is any part of it.
			slog.WarnContext(r.Context(), "oracle: missing or wrong token",
				slog.String("path", r.URL.Path), slog.String("remote", r.RemoteAddr))
			w.Header().Set("WWW-Authenticate", `Bearer realm="oracle"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) ping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) evaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	api.SetJSONReadDeadline(w)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			metrics.OracleRequests.WithLabelValues("bad_request").Inc()
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		metrics.OracleRequests.WithLabelValues("bad_request").Inc()
		slog.WarnContext(r.Context(), "oracle: the request body could not be read", slog.Any("error", err))
		writeError(w, http.StatusBadRequest, "unreadable_body")
		return
	}
	req, err := DecodeBytes(raw)
	if err != nil {
		metrics.OracleRequests.WithLabelValues("bad_request").Inc()
		var bad *InvalidRequestError
		if errors.As(err, &bad) {
			slog.WarnContext(r.Context(), "oracle: invalid request", slog.String("reason", bad.Reason))
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "detail": bad.Reason})
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	requestID, _ := r.Context().Value(api.RequestIDKey).(string)
	res, err := h.svc.Evaluate(r.Context(), req, raw, requestID)
	switch {
	case err == nil:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, context.Canceled):
		// The caller went away; nobody reads the answer. The work finishes anyway.
		slog.InfoContext(r.Context(), "oracle: the caller stopped waiting", slog.String("signal_id", req.Signal.SignalID))
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "model_timeout")
	case errors.Is(err, ErrModelUnavailable):
		writeError(w, http.StatusBadGateway, "model_unavailable")
	case errors.Is(err, ErrUnusableReply):
		writeError(w, http.StatusBadGateway, "unusable_reply")
	default:
		slog.ErrorContext(r.Context(), "oracle: evaluation failed", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}

type decisionsResponse struct {
	Decisions []Evaluation `json:"decisions"`
	// NextBeforeID is the before_id of the next page; 0 when this was the last one.
	NextBeforeID int64 `json:"next_before_id"`
}

// decisions lists the journal, newest first. It is behind the ordinary user check:
// the journal belongs to the system, not to a user, and the system has one owner.
func (h *Handler) decisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	q := r.URL.Query()
	limit, ok := intParam(q.Get("limit"), defaultListLimit, 1, maxListLimit)
	if !ok {
		http.Error(w, "limit must be a whole number from 1 to "+strconv.Itoa(maxListLimit), http.StatusBadRequest)
		return
	}
	var beforeID int64
	if raw := q.Get("before_id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "before_id must be a whole number, 0 or more", http.StatusBadRequest)
			return
		}
		beforeID = n
	}
	symbol := strings.ToUpper(strings.TrimSpace(q.Get("symbol")))
	if symbol != "" && !symbolPattern.MatchString(symbol) {
		http.Error(w, "symbol is not a stock symbol", http.StatusBadRequest)
		return
	}

	rows, err := h.store.List(r.Context(), ListFilter{Symbol: symbol, BeforeID: beforeID, Limit: limit})
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to list the oracle journal", slog.Any("error", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	resp := decisionsResponse{Decisions: rows}
	if len(rows) == limit {
		resp.NextBeforeID = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// intParam parses a whole number in [lo, hi]; an empty value is def.
func intParam(raw string, def, lo, hi int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("failed to write the oracle response", slog.Any("error", err))
	}
}
