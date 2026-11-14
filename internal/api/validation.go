package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

const (
	// maxJSONBodyBytes bounds every JSON request body served by this package.
	// The payloads are text, so 1 MiB sits far above any legitimate request
	// while stopping one connection from allocating hundreds of megabytes.
	maxJSONBodyBytes = 1 << 20

	// maxSessionIDChars bounds the session key. It is used as an index key and
	// is never legitimately long.
	maxSessionIDChars = 128

	// maxContentChars bounds a single message or memory body. Anything longer is
	// dropped by the prompt budget anyway, so accepting it only wastes storage.
	maxContentChars = 32000

	// minImportance/maxImportance mirror the meaning of the importance column:
	// 1 = trivia, 10 = critical.
	minImportance = 1
	maxImportance = 10

	allowedRolesDescription    = "user, assistant, system"
	allowedStatusesDescription = "candidate, active"
)

// allowedRoles holds the values messages.role accepts. The prompt builder and
// the provider only understand these three.
var allowedRoles = map[string]struct{}{
	"user":      {},
	"assistant": {},
	"system":    {},
}

// allowedMemoryStatuses holds the lifecycle states a memory may be stored in.
var allowedMemoryStatuses = map[string]struct{}{
	"candidate":  {},
	"active":     {},
}

// decodeJSON decodes a size-limited JSON body into dst. It writes the response
// itself and reports false when the caller must stop.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	SetJSONReadDeadline(w)

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

	writeInvalidBody(w, r, err)
	return false
}

// validateContent rejects blank or oversized message and memory bodies.
func validateContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("content is required")
	}
	if len(content) > maxContentChars {
		return fmt.Errorf("content must be at most %d characters", maxContentChars)
	}
	return nil
}

// validateSessionID rejects blank or oversized session keys.
func validateSessionID(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session_id is required")
	}
	if len(sessionID) > maxSessionIDChars {
		return fmt.Errorf("session_id must be at most %d characters", maxSessionIDChars)
	}
	return nil
}

// validateImportance keeps importance inside the range the column documents.
func validateImportance(importance int32) error {
	if importance < minImportance || importance > maxImportance {
		return fmt.Errorf("importance must be between %d and %d", minImportance, maxImportance)
	}
	return nil
}

// validateMessageInput checks the fields CreateMessageHandler stores verbatim.
func validateMessageInput(sessionID, role, content string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	if _, ok := allowedRoles[role]; !ok {
		return fmt.Errorf("role must be one of: %s", allowedRolesDescription)
	}
	return validateContent(content)
}

// validateMemoryInput checks the fields a memory write stores verbatim. The
// caller is expected to have applied the default status already.
func validateMemoryInput(content, status string, importance int32) error {
	if err := validateContent(content); err != nil {
		return err
	}
	if _, ok := allowedMemoryStatuses[status]; !ok {
		return fmt.Errorf("status must be one of: %s", allowedStatusesDescription)
	}
	return validateImportance(importance)
}
