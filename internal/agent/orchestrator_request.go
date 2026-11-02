package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"go-monolith/internal/api"
)

// ValidateUserMessage is validateUserMessage for callers outside the package
// (the Telegram bot), so every entry point enforces the same limit.
func ValidateUserMessage(content string) error {
	return validateUserMessage(content)
}

// validateUserMessage rejects turns the prompt budget cannot carry. The same
// estimateTokens heuristic as the history budget is used, so "too long" means
// the same thing everywhere.
func validateUserMessage(content string) error {
	if tokens := estimateTokens(content); tokens > maxUserMessageTokens {
		return fmt.Errorf("message must be at most %d tokens", maxUserMessageTokens)
	}
	return nil
}

// readJSONBody decodes a size-limited JSON request body into dst. It answers the
// request itself and reports false when the caller must stop.
func readJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	api.SetJSONReadDeadline(w)

	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		slog.WarnContext(r.Context(), "request body too large",
			slog.Int64("limit", tooLarge.Limit),
			slog.String("path", r.URL.Path))
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}

	// The decoding error quotes struct field names and byte offsets, so it is
	// logged rather than echoed back to the caller.
	slog.WarnContext(r.Context(), "invalid request body", slog.Any("error", err), slog.String("path", r.URL.Path))
	http.Error(w, "invalid request body", http.StatusBadRequest)
	return false
}
