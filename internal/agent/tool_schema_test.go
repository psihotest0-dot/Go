package agent

import (
	"encoding/json"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stricter providers than llama-server check the schema of every tool: DeepSeek answered 400
// "Invalid schema for function 'list_files': null is not of type array" to a tool without a
// required argument (2026-11-14). So every tool offered, in every group, must have an object
// schema whose "required" is a list when it is there at all.
func TestEveryToolSchemaIsValidForAStrictProvider(t *testing.T) {
	var all []openai.Tool
	all = append(all, toolDefinitions()...)
	all = append(all, fileToolDefinitions()...)
	all = append(all, filesStubDefinition())

	for _, tool := range all {
		require.NotNil(t, tool.Function)
		t.Run(tool.Function.Name, func(t *testing.T) {
			raw, err := json.Marshal(tool.Function.Parameters)
			require.NoError(t, err)
			var schema map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &schema))

			assert.JSONEq(t, `"object"`, string(schema["type"]))
			assert.Contains(t, schema, "properties", "an object schema lists its properties, even if there are none")
			if required, ok := schema["required"]; ok {
				var list []string
				assert.NoError(t, json.Unmarshal(required, &list), "required is a list of names, not null: %s", required)
				assert.NotEmpty(t, list, "an empty list of required arguments is left out")
			}
		})
	}
}
