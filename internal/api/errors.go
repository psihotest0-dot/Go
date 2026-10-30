package api

import (
	"log/slog"
	"net/http"
)

// internalErrorMessage is the only text clients ever see for a server-side
// failure. Driver and upstream errors routinely carry SQL statements, table
// names, credentials in DSNs and internal host names, none of which belong in a
// response body.
const internalErrorMessage = "internal server error"

// writeInternalError logs err with its operation context and answers with a
// generic 500 so the details stay in the server logs.
func writeInternalError(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, slog.Any("error", err))
	http.Error(w, internalErrorMessage, http.StatusInternalServerError)
}

// writeInvalidBody answers a malformed request body. The decoding error itself
// is logged with the request path but not echoed: it quotes struct field names
// and offsets that mean nothing to the client.
func writeInvalidBody(w http.ResponseWriter, path string, err error) {
	slog.Warn("invalid request body", slog.Any("error", err), slog.String("path", path))
	http.Error(w, "invalid request body", http.StatusBadRequest)
}
