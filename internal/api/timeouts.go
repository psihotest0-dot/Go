package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Body read deadlines are applied per request, at the place where the body is
// read, and not through http.Server.ReadTimeout: that timeout spans the whole
// request, so a large upload over a slow disk or link was cut off after 30 s.
// They are variables so tests can shorten them.
var (
	// jsonReadTimeout bounds how long a JSON request body may take to arrive.
	jsonReadTimeout = 30 * time.Second

	// uploadReadTimeout bounds a whole file upload (100 MB at 1 Mbit/s is ~14 min).
	uploadReadTimeout = 30 * time.Minute
)

// NewHTTPServer returns the server the application runs. It deliberately has no
// ReadTimeout (see above); ReadHeaderTimeout still stops slow-header clients.
func NewHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// SetJSONReadDeadline limits how long the rest of a JSON request body may take.
func SetJSONReadDeadline(w http.ResponseWriter) {
	setReadDeadline(w, jsonReadTimeout)
}

func setReadDeadline(w http.ResponseWriter, d time.Duration) {
	err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
	// Writers that cannot do this (httptest recorders) are not an error.
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("failed to set the request read deadline", slog.Any("error", err))
	}
}
