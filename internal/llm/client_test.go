package llm

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestContextManager_Pruning(t *testing.T) {
	mock := &MockLLM{}
	cm := NewContextManager(mock, 3)

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "You are a helpful assistant."},
		{Role: openai.ChatMessageRoleUser, Content: "Message 1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Response 1"},
		{Role: openai.ChatMessageRoleUser, Content: "Message 2"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Response 2"},
		{Role: openai.ChatMessageRoleUser, Content: "Message 3"},
	}

	req := openai.ChatCompletionRequest{Messages: messages}
	_, _ = cm.Chat(context.Background(), req)

	if len(mock.LastRequest.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(mock.LastRequest.Messages))
	}

	if mock.LastRequest.Messages[0].Role != openai.ChatMessageRoleSystem {
		t.Errorf("expected system message to be preserved")
	}

	if mock.LastRequest.Messages[2].Content != "Message 3" {
		t.Errorf("expected last message to be 'Message 3', got %s", mock.LastRequest.Messages[2].Content)
	}
}

func TestContextManager_Pruning_NoSystemPrompt(t *testing.T) {
	mock := &MockLLM{}
	cm := NewContextManager(mock, 2)

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "Message 1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Response 1"},
		{Role: openai.ChatMessageRoleUser, Content: "Message 2"},
	}

	req := openai.ChatCompletionRequest{Messages: messages}
	_, _ = cm.Chat(context.Background(), req)

	if len(mock.LastRequest.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(mock.LastRequest.Messages))
	}

	if mock.LastRequest.Messages[0].Content != "Response 1" {
		t.Errorf("expected first message to be 'Response 1', got %s", mock.LastRequest.Messages[0].Content)
	}

	if mock.LastRequest.Messages[1].Content != "Message 2" {
		t.Errorf("expected last message to be 'Message 2', got %s", mock.LastRequest.Messages[1].Content)
	}
}

func TestContextManager_Stream(t *testing.T) {
	mock := &MockLLM{
		StreamChunks: []openai.ChatCompletionStreamResponse{
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "Hello"}}}},
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: " World"}}}},
		},
	}
	cm := NewContextManager(mock, 10)

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "Hi"},
		},
	}

	stream, err := cm.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer stream.Close()

	var result string
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		result += resp.Choices[0].Delta.Content
	}

	if result != "Hello World" {
		t.Errorf("expected 'Hello World', got '%s'", result)
	}
}
