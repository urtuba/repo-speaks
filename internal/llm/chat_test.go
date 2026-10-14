package llm

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// chatRequest is a request with each kind of message and one tool.
var chatRequest = ChatRequest{
	ModelID: "z-ai/glm-5.3-flash",
	Messages: []Message{
		{Role: RoleSystem, Content: "Use the tools."},
		{Role: RoleUser, Content: "How do refunds work?"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "search_text", Arguments: `{"query":"refund"}`}}},
		{Role: RoleTool, ToolCallID: "c1", Content: "app/refund.go:3: func Refund()"},
	},
	Tools: []Tool{{
		Name: "search_text", Description: "Searches the files.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}},
	MaxTokens: 1000,
}

func TestChatRequestBody(t *testing.T) {
	ctx := context.Background()
	h, url := newFakeHost(t)
	h.chat = `{"choices": [{"message": {"content": "Refunds take 5 days."}, "finish_reason": "stop"}]}`
	c := New(true)
	if _, err := c.Chat(ctx, url+"/v1", testKey, chatRequest); err != nil {
		t.Fatal(err)
	}
	if h.auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the key as a bearer token", h.auth)
	}
	raw, err := json.Marshal(h.gotBody)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	json.Unmarshal([]byte(`{
		"model": "z-ai/glm-5.3-flash",
		"messages": [
			{"role": "system", "content": "Use the tools."},
			{"role": "user", "content": "How do refunds work?"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "search_text", "arguments": "{\"query\":\"refund\"}"}}]},
			{"role": "tool", "tool_call_id": "c1", "content": "app/refund.go:3: func Refund()"}
		],
		"tools": [{"type": "function", "function": {"name": "search_text", "description": "Searches the files.",
			"parameters": {"type": "object", "properties": {}}}}],
		"max_tokens": 1000
	}`), &want)
	if !reflect.DeepEqual(h.gotBody, want) {
		t.Errorf("request body =\n%s\nwant\n%v", raw, want)
	}

	// NoTools keeps the tools listed and tells the model not to use them.
	req := chatRequest
	req.NoTools = true
	if _, err := c.Chat(ctx, url+"/v1", testKey, req); err != nil {
		t.Fatal(err)
	}
	if h.gotBody["tool_choice"] != "none" || h.gotBody["tools"] == nil {
		t.Errorf("with NoTools: tool_choice = %v, tools = %v", h.gotBody["tool_choice"], h.gotBody["tools"])
	}
}

func TestChatReply(t *testing.T) {
	ctx := context.Background()
	h, url := newFakeHost(t)
	c := New(true)
	tests := []struct {
		name string
		chat string
		want ChatReply
	}{
		{
			"text",
			`{"choices": [{"message": {"role": "assistant", "content": "Refunds take 5 days."}, "finish_reason": "stop"}],
			  "usage": {"prompt_tokens": 1200, "completion_tokens": 30}}`,
			ChatReply{
				Message:      Message{Role: RoleAssistant, Content: "Refunds take 5 days."},
				FinishReason: "stop", PromptTokens: 1200, CompletionTokens: 30,
			},
		},
		{
			"tool calls, one without an ID",
			`{"choices": [{"message": {"content": null, "tool_calls": [
				{"id": "a", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"x\"}"}},
				{"type": "function", "function": {"name": "list_files", "arguments": "{}"}}]},
			  "finish_reason": "tool_calls"}], "usage": {"prompt_tokens": 50, "completion_tokens": 5}}`,
			ChatReply{
				Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
					{ID: "a", Name: "read_file", Arguments: `{"path":"x"}`},
					{ID: "call_2", Name: "list_files", Arguments: "{}"},
				}},
				FinishReason: "tool_calls", PromptTokens: 50, CompletionTokens: 5,
			},
		},
		{
			"content as parts",
			`{"choices": [{"message": {"content": [{"type": "text", "text": "One. "}, {"type": "image"}, {"type": "text", "text": "Two."}]},
			  "finish_reason": "stop"}]}`,
			ChatReply{Message: Message{Role: RoleAssistant, Content: "One. Two."}, FinishReason: "stop"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.chat = tt.chat
			got, err := c.Chat(ctx, url+"/v1", testKey, chatRequest)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Chat() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestChatErrors(t *testing.T) {
	ctx := context.Background()
	h, url := newFakeHost(t)
	c := New(true)
	tests := []struct {
		name   string
		status int
		chat   string
		want   string
	}{
		{"not JSON", 200, `<html>`, "the host's reply is not a chat completion: check the base URL"},
		{"no choices", 200, `{"choices": []}`, "the host's reply has no answer"},
		{"error inside a 200", 200, `{"error": {"message": "Rate limit for ` + testKey + `"}}`,
			"the host answered with an error: Rate limit for [hidden]"},
		{"bad key, echoed back", 401, `{"error": {"message": "Key ` + testKey + ` is not valid"}}`,
			"the host answered 401 Unauthorized: Key [hidden] is not valid"},
		{"raw body is never shown", 502, `<html>upstream ` + testKey + ` failed</html>`,
			"the host answered 502 Bad Gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.status, h.chat = tt.status, tt.chat
			_, err := c.Chat(ctx, url+"/v1", testKey, chatRequest)
			if err == nil || err.Error() != tt.want {
				t.Errorf("Chat() error = %v\nwant %s", err, tt.want)
			}
			checkSafe(t, err)
		})
	}

	// A reply over the limit.
	h.status, h.chat = 200, strings.Repeat(" ", maxReplyBytes+1)
	if _, err := c.Chat(ctx, url+"/v1", testKey, chatRequest); err == nil || !strings.Contains(err.Error(), "over the 1 MB limit") {
		t.Errorf("large reply: error = %v", err)
	}

	// The guard applies to chat calls too.
	_, err := New(false).Chat(ctx, url+"/v1", testKey, chatRequest)
	if err == nil || !strings.Contains(err.Error(), "is private, and this server blocks private addresses") {
		t.Errorf("private host: error = %v", err)
	}
	checkSafe(t, err)
}
