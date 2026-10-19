package agent

import (
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/prompts"
)

func buildSystemPrompt(ctx chatContext) (openai.ChatCompletionMessage, error) {
	systemContent, err := prompts.GlobalManager.Render("chat_system.tmpl", map[string]interface{}{
		"Rules":            ctx.Rules,
		"TopMemories":      ctx.TopMemories,
		"RollingSummary":   ctx.RollingSum,
		"RelevantMemories": ctx.Memories,
		"Chunks":           ctx.Chunks,
	})
	if err != nil {
		return openai.ChatCompletionMessage{}, err
	}

	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemContent,
	}, nil
}

func toolDefinitions() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "create_memory",
				Description: "Save a new personal fact or preference about the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The explicit fact about the user in 3rd person.",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"description": "The importance of the fact from 1 to 10.",
						},
					},
					"required": []string{"content"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "search_memories",
				Description: "Search for past context or personal details about the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The text to search for.",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "The maximum number of results to return (default 5, max 20).",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "search_documents",
				Description: "Search for context, information, or answers within the uploaded documents and files.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The text to search for.",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "The maximum number of results to return (default 5, max 20).",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "save_global_rule",
				Description: "Save a new global rule or preference (e.g., 'always speak in Bulgarian'). Overwrites existing rule with same key.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{
							"type":        "string",
							"description": "The short key for the rule (e.g., 'language', 'tone').",
						},
						"value": map[string]interface{}{
							"type":        "string",
							"description": "The rule instruction value.",
						},
					},
					"required": []string{"key", "value"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "web_search",
				Description: "Search the web for a query and return a ranked list of structured results.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The search query. Supports operators such as site:, -site:, and quoted phrases. Must contain 1 to 128 Unicode characters.",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "web_fetch",
				Description: "Fetch a web page and return its title and content as Markdown.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url": map[string]interface{}{
							"type":        "string",
							"description": "The URL to fetch. Must be at most 8192 characters and an HTTP or HTTPS URL with a host.",
						},
					},
					"required": []string{"url"},
				},
			},
		},
	}
}
