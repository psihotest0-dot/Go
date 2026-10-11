package agent

import "context"

// ToolRegistry defines the interface for tool execution.
type ToolRegistry interface {
	ExecuteTool(ctx context.Context, name string, args map[string]interface{}) (interface{}, error)
}

// MockToolRegistry is a mock implementation of ToolRegistry for testing.
type MockToolRegistry struct{}

// ExecuteTool mocks a tool execution by returning the input args or a static value.
func (m *MockToolRegistry) ExecuteTool(ctx context.Context, name string, args map[string]interface{}) (interface{}, error) {
	return map[string]interface{}{"status": "success", "mocked_tool": name, "args": args}, nil
}
