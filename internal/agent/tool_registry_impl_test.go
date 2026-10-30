package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
)

// toolContext builds the context a tool call runs in: the owner of the request
// plus the user message that triggered the turn.
func toolContext(userMessage string) context.Context {
	ctx := context.WithValue(context.Background(), api.ParsedUserIDKey, uuid.New())
	return WithCurrentQuery(ctx, userMessage)
}

func TestSearchQueryArgPrefersTheModelArgument(t *testing.T) {
	query, err := searchQueryArg(toolContext("разкажи ми за проекта"), "search_memories",
		map[string]interface{}{"query": "  проект  "})

	require.NoError(t, err)
	assert.Equal(t, "проект", query, "the explicit argument is trimmed and wins over the fallback")
}

func TestSearchQueryArgFallsBackToCurrentUserMessage(t *testing.T) {
	query, err := searchQueryArg(toolContext("какво знаеш за мен"), "search_documents",
		map[string]interface{}{"query": "   "})

	require.NoError(t, err)
	assert.Equal(t, "какво знаеш за мен", query)
}

func TestSearchQueryArgWithoutAnyTextIsAnArgumentError(t *testing.T) {
	_, err := searchQueryArg(context.Background(), "search_memories", map[string]interface{}{})

	require.Error(t, err)
	var argErr *toolArgumentError
	assert.ErrorAs(t, err, &argErr)
	assert.Contains(t, err.Error(), `"query"`)
	assert.Contains(t, err.Error(), "search_memories")
}

func TestSearchMemoriesToolUsesTheCurrentUserMessageAsQuery(t *testing.T) {
	// A conversational filler message short-circuits the search before it
	// touches the database, so the fallback can be asserted without retrieval.
	registry := &DefaultToolRegistry{}

	res, err := registry.ExecuteTool(toolContext("здравей"), "search_memories", map[string]interface{}{})
	require.NoError(t, err)

	payload, ok := res.(map[string]interface{})
	require.True(t, ok, "unexpected result type %T", res)
	assert.Equal(t, "skipped", payload["status"])
}

func TestSearchDocumentsToolWithoutUserMessageIsAnArgumentError(t *testing.T) {
	registry := &DefaultToolRegistry{}

	_, err := registry.ExecuteTool(toolContext(""), "search_documents", nil)

	require.Error(t, err)
	var argErr *toolArgumentError
	assert.ErrorAs(t, err, &argErr)
}

func TestIntArgAcceptsTheTypesModelsSend(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		want  int
	}{
		{name: "float64", value: float64(3), want: 3},
		{name: "json number", value: json.Number("2"), want: 2},
		{name: "numeric string", value: "4", want: 4},
		{name: "not a number", value: "many", want: 5},
		{name: "zero", value: float64(0), want: 5},
		{name: "missing", value: nil, want: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]interface{}{}
			if tt.value != nil {
				args["limit"] = tt.value
			}
			assert.Equal(t, tt.want, intArg(args, "limit", 5))
		})
	}
}
