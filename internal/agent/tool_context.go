package agent

import "context"

// currentQueryKey stores the user message that started the current turn. Tool
// calls carry only the arguments the model chose to send, so a model that
// omits a required argument leaves the registry without the text the user
// actually asked about. Keeping the message in the context lets the search
// tools fall back to it instead of failing the whole turn.
type currentQueryKey struct{}

// WithCurrentQuery records the user message of the current turn in ctx.
func WithCurrentQuery(ctx context.Context, query string) context.Context {
	if query == "" {
		return ctx
	}
	return context.WithValue(ctx, currentQueryKey{}, query)
}

// currentQuery returns the user message recorded by WithCurrentQuery, or an
// empty string when the caller did not provide one.
func currentQuery(ctx context.Context) string {
	query, _ := ctx.Value(currentQueryKey{}).(string)
	return query
}
