package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// maxLoggedHeaderRunes caps a client supplied header value written to the log.
const maxLoggedHeaderRunes = 80

// contextHandler adds the request_id and the user_id of the request a log call
// belongs to. The request middleware puts both into the context, so a handler
// only has to use slog.ErrorContext(r.Context(), ...) and the line can be found
// by the X-Request-ID the client received.
type contextHandler struct {
	slog.Handler
}

// NewContextHandler wraps next so that log calls made with a request context
// carry its request_id and user_id.
func NewContextHandler(next slog.Handler) slog.Handler {
	return contextHandler{Handler: next}
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := ctx.Value(RequestIDKey).(string); ok && id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	// Only the parsed id: the raw header is client text of any length.
	if id, ok := ctx.Value(ParsedUserIDKey).(uuid.UUID); ok {
		r.AddAttrs(slog.String("user_id", id.String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
