package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testKey = "sk-or-v1-0000secretvalue0000-k3y9"

// fakeHost is an OpenAI-compatible host under /v1. Tests set what it
// answers. The httptest server runs on 127.0.0.1, so the tests use a
// client that allows private addresses; guard_test.go tests the guard.
type fakeHost struct {
	mu        sync.Mutex
	models    string // the body of GET /v1/models
	status    int    // the status of POST /v1/chat/completions
	chat      string // its body
	auth      string // the Authorization header of the last request
	gotBody   map[string]any
	redirects int // requests to /moved, which only a redirect leads to
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.auth = r.Header.Get("Authorization")
	switch r.URL.Path {
	case "/v1/models":
		io.WriteString(w, h.models)
	case "/v1/chat/completions":
		h.gotBody = nil
		json.NewDecoder(r.Body).Decode(&h.gotBody)
		w.WriteHeader(h.status)
		io.WriteString(w, h.chat)
	case "/old/models":
		http.Redirect(w, r, "/moved", http.StatusMovedPermanently)
	case "/moved":
		h.redirects++
		io.WriteString(w, h.models)
	default:
		http.NotFound(w, r)
	}
}

func newFakeHost(t *testing.T) (*fakeHost, string) {
	t.Helper()
	h := &fakeHost{
		models: `{"data": [
			{"id": "z-ai/glm-5.3-flash", "supported_parameters": ["max_tokens", "tools", "tool_choice"]},
			{"id": "old/no-tools", "supported_parameters": ["max_tokens"]},
			{"id": "local/llama"}
		]}`,
		status: http.StatusOK,
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

// checkSafe fails the test when err's message holds the key.
func checkSafe(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "secretvalue") {
		t.Errorf("error holds the key: %v", err)
	}
	if err != nil {
		if _, ok := err.(*Error); !ok {
			t.Errorf("error is %T, want *Error", err)
		}
	}
}

func TestListModels(t *testing.T) {
	ctx := context.Background()
	h, url := newFakeHost(t)
	c := New(true)

	list, err := c.ListModels(ctx, url+"/v1", testKey)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "z-ai/glm-5.3-flash", ToolsKnown: true, Tools: true},
		{ID: "old/no-tools", ToolsKnown: true, Tools: false},
		{ID: "local/llama"},
	}
	if fmt.Sprint(list) != fmt.Sprint(want) {
		t.Errorf("ListModels() = %+v, want %+v", list, want)
	}
	if h.auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the key as a bearer token", h.auth)
	}

	// No key, no header.
	if _, err := c.ListModels(ctx, url+"/v1", ""); err != nil {
		t.Fatal(err)
	}
	if h.auth != "" {
		t.Errorf("Authorization without a key = %q, want none", h.auth)
	}
}

func TestListModelsErrors(t *testing.T) {
	ctx := context.Background()
	h, url := newFakeHost(t)
	c := New(true)

	tests := []struct {
		name   string
		models string
		base   string
		want   string
	}{
		{"a web page", "<html>hello</html>", url + "/v1", "not a model list"},
		{"JSON without data", `{"object": "list"}`, url + "/v1", "not a model list"},
		{"wrong path", "", url + "/v2", "404 Not Found"},
		{"a redirect", `{"data": []}`, url + "/old", "301 Moved Permanently, a redirect, which is not followed"},
		{"over the size limit", `{"data": [` + strings.Repeat(" ", maxListBytes) + `]}`, url + "/v1", "over the 10 MB limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.models = tt.models
			_, err := c.ListModels(ctx, tt.base, testKey)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ListModels() error = %v, want %q", err, tt.want)
			}
			checkSafe(t, err)
		})
	}
	if h.redirects != 0 {
		t.Errorf("the client followed %d redirects, want none", h.redirects)
	}

	// Nothing listens on port 1.
	_, err := c.ListModels(ctx, "http://127.0.0.1:1/v1", testKey)
	if err == nil || err.Error() != "the host refused the connection" {
		t.Errorf("closed port: error = %v", err)
	}
	_, err = c.ListModels(ctx, "https://no-such-host.invalid/v1", testKey)
	if err == nil || err.Error() != "the host name was not found" {
		t.Errorf("unknown name: error = %v", err)
	}
}

func TestCheckModel(t *testing.T) {
	ctx := context.Background()
	_, url := newFakeHost(t)
	c := New(true)

	tests := []struct {
		id   string
		want string // part of the error, or "" for none
	}{
		{"z-ai/glm-5.3-flash", ""},
		{"local/llama", ""}, // the host does not say, so the Test button checks tools
		{"old/no-tools", `the host says model "old/no-tools" does not support tool use`},
		{"z-ai/glm-5.3", `the host does not list a model with the ID "z-ai/glm-5.3"`},
		{"Z-AI/GLM-5.3-FLASH", "does not list"},
	}
	for _, tt := range tests {
		err := c.CheckModel(ctx, url+"/v1", testKey, tt.id)
		if tt.want == "" {
			if err != nil {
				t.Errorf("CheckModel(%q) = %v, want nil", tt.id, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("CheckModel(%q) = %v, want %q", tt.id, err, tt.want)
		}
	}
}

func TestTestCall(t *testing.T) {
	ctx := context.Background()
	h, url := newFakeHost(t)
	c := New(true)
	const toolCall = `{"choices": [{"message": {"tool_calls": [{"id": "1", "type": "function",
		"function": {"name": "get_time", "arguments": "{}"}}]}, "finish_reason": "tool_calls"}]}`

	h.chat = toolCall
	if err := c.Test(ctx, url+"/v1", testKey, "z-ai/glm-5.3-flash"); err != nil {
		t.Fatalf("Test() = %v, want nil", err)
	}
	if h.auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the key as a bearer token", h.auth)
	}
	body := h.gotBody
	tools, _ := body["tools"].([]any)
	if body["model"] != "z-ai/glm-5.3-flash" || len(tools) != 1 || body["max_tokens"] != float64(testMaxTokens) {
		t.Errorf("request body = %v", body)
	}
	if !strings.Contains(fmt.Sprint(tools), "get_time") {
		t.Errorf("tools = %v, want get_time", tools)
	}

	long := strings.Repeat("x", 500)
	tests := []struct {
		name   string
		status int
		chat   string
		want   string
	}{
		{"no tool call", 200, `{"choices": [{"message": {"content": "It is noon."}, "finish_reason": "stop"}]}`,
			"the model answered without calling the tool, so it may not support tool use"},
		{"out of tokens", 200, `{"choices": [{"message": {"content": ""}, "finish_reason": "length"}]}`,
			"the model ran out of tokens before it called the tool"},
		{"no choices", 200, `{"choices": []}`, "the host's reply has no answer"},
		{"not JSON", 200, `<html>`, "the host's reply is not a chat completion: check the base URL"},
		{"error inside a 200", 200, `{"error": {"message": "No endpoints found that support tool use."}}`,
			"the host answered with an error: No endpoints found that support tool use."},
		{"bad key, echoed back", 401, `{"error": {"message": "Key ` + testKey + ` is not valid", "code": 401}}`,
			"the host answered 401 Unauthorized: Key [hidden] is not valid"},
		{"error as a string", 400, `{"error": "model not found"}`,
			"the host answered 400 Bad Request: model not found"},
		{"long message", 429, `{"error": {"message": "` + long + `"}}`,
			"the host answered 429 Too Many Requests: " + long[:maxHostMessage] + "..."},
		{"control characters", 500, `{"error": {"message": "line one\nline\ttwo\u0000"}}`,
			"the host answered 500 Internal Server Error: line one line two"},
		{"raw body is never shown", 502, `<html>upstream ` + testKey + ` failed</html>`,
			"the host answered 502 Bad Gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.status, h.chat = tt.status, tt.chat
			err := c.Test(ctx, url+"/v1", testKey, "z-ai/glm-5.3-flash")
			if err == nil || err.Error() != tt.want {
				t.Errorf("Test() error = %v\nwant %s", err, tt.want)
			}
			checkSafe(t, err)
		})
	}

	// A reply over the limit.
	h.status, h.chat = 200, strings.Repeat(" ", maxReplyBytes+1)
	if err := c.Test(ctx, url+"/v1", testKey, "m"); err == nil || !strings.Contains(err.Error(), "over the 1 MB limit") {
		t.Errorf("large reply: error = %v", err)
	}
}

// TestKeyNeverInErrors runs every way a call can fail with a host that
// puts the key everywhere it can.
func TestKeyNeverInErrors(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("X-Key", key)
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprintf(w, `{"data": "%s"}`, key)
		case "/redirect/models":
			http.Redirect(w, r, "/"+key, http.StatusFound)
		default:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error": {"message": "%s %s %s"}}`, key, key, strings.Repeat(key, 20))
		}
	}))
	defer srv.Close()
	c := New(true)
	var errs []error
	_, err := c.ListModels(ctx, srv.URL+"/v1", testKey)
	errs = append(errs, err)
	_, err = c.ListModels(ctx, srv.URL+"/redirect", testKey)
	errs = append(errs, err)
	errs = append(errs, c.CheckModel(ctx, srv.URL+"/v1", testKey, "m"))
	errs = append(errs, c.Test(ctx, srv.URL+"/v1", testKey, "m"))
	errs = append(errs, c.Test(ctx, "http://127.0.0.1:1/v1", testKey, "m"))
	errs = append(errs, New(false).Test(ctx, srv.URL+"/v1", testKey, "m"))
	for i, err := range errs {
		if err == nil {
			t.Errorf("call %d: no error", i)
		}
		checkSafe(t, err)
	}
}
