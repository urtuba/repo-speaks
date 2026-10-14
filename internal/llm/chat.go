package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// The roles of a chat message.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool" // the result of a tool call
)

// Message is one message of a chat with a model.
type Message struct {
	Role    string
	Content string
	// ToolCalls are the tools an assistant message asks for.
	ToolCalls []ToolCall
	// ToolCallID names the call a tool message answers.
	ToolCallID string
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON, as the model wrote it; it may be broken
}

// Tool is a tool the model may call. Parameters is a JSON Schema.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ChatRequest is one call of a chat.
type ChatRequest struct {
	ModelID  string
	Messages []Message
	Tools    []Tool
	// NoTools tells the model to answer in text, while the tools stay
	// listed. Some hosts refuse a chat with tool calls in it when no
	// tools are listed.
	NoTools   bool
	MaxTokens int // 0 for the host's default
}

// ChatReply is the model's reply to one call.
type ChatReply struct {
	Message          Message // the assistant's message
	FinishReason     string  // such as "stop", "tool_calls" or "length"
	PromptTokens     int
	CompletionTokens int
}

// The JSON forms of the OpenAI-compatible API.
type (
	wireMessage struct {
		Role string `json:"role"`
		// Content is null in an assistant message that only calls tools.
		Content    *string        `json:"content"`
		ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
	}
	wireToolCall struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	wireTool struct {
		Type     string `json:"type"`
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	}
	wireRequest struct {
		Model      string        `json:"model"`
		Messages   []wireMessage `json:"messages"`
		Tools      []wireTool    `json:"tools,omitempty"`
		ToolChoice string        `json:"tool_choice,omitempty"`
		MaxTokens  int           `json:"max_tokens,omitempty"`
	}
)

// Chat sends the messages to the model and returns its reply. The reply
// holds either text or tool calls, or both. It follows the same rules as
// the other calls: no redirects, a capped reply and safe errors. A call
// may take up to 2 minutes.
func (c *Client) Chat(ctx context.Context, baseURL, key string, req ChatRequest) (ChatReply, error) {
	body, err := json.Marshal(wireChat(req))
	if err != nil {
		return ChatReply{}, &Error{"could not build the request"}
	}
	data, err := c.call(ctx, c.chat, http.MethodPost, baseURL+"/chat/completions", key, body, maxReplyBytes)
	if err != nil {
		return ChatReply{}, err
	}
	var reply struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []wireToolCall  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return ChatReply{}, &Error{"the host's reply is not a chat completion: check the base URL"}
	}
	if err := replyError(reply.Error, key); err != nil {
		return ChatReply{}, err
	}
	if len(reply.Choices) == 0 {
		return ChatReply{}, &Error{"the host's reply has no answer"}
	}
	choice := reply.Choices[0]
	out := ChatReply{
		Message:          Message{Role: RoleAssistant, Content: contentText(choice.Message.Content)},
		FinishReason:     choice.FinishReason,
		PromptTokens:     reply.Usage.PromptTokens,
		CompletionTokens: reply.Usage.CompletionTokens,
	}
	for i, tc := range choice.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			// Some local hosts send no ID. The app must name the call in
			// its answer, so it makes one up.
			id = "call_" + strconv.Itoa(i+1)
		}
		out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
			ID: id, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	return out, nil
}

// wireChat turns a request into its JSON form.
func wireChat(req ChatRequest) wireRequest {
	w := wireRequest{Model: req.ModelID, MaxTokens: req.MaxTokens}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		if m.Content != "" || len(m.ToolCalls) == 0 {
			wm.Content = &m.Content
		}
		for _, tc := range m.ToolCalls {
			var wt wireToolCall
			wt.ID, wt.Type = tc.ID, "function"
			wt.Function.Name, wt.Function.Arguments = tc.Name, tc.Arguments
			wm.ToolCalls = append(wm.ToolCalls, wt)
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name, wt.Function.Description, wt.Function.Parameters = t.Name, t.Description, t.Parameters
		w.Tools = append(w.Tools, wt)
	}
	if req.NoTools && len(w.Tools) > 0 {
		w.ToolChoice = "none"
	}
	return w
}

// contentText reads a message's content: a string, null, or a list of
// parts, of which it keeps the text.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
