// Package llm talks to LLM hosts through the OpenAI-compatible API. Any
// host that speaks it works: OpenRouter, a vendor's own API, or a local
// server such as Ollama. It knows nothing else of the app.
//
// The base URL of a host is set by a project admin, who may not run the
// server. So the client refuses private addresses unless the operator
// allows them, follows no redirects, ignores proxy settings, and caps
// time and reply sizes.
package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// callTimeout limits one call, from connecting to the last byte.
	callTimeout = 30 * time.Second

	// chatTimeout limits one call of an agent's answer. The model reads a
	// long context and may think before it answers, so it gets longer.
	chatTimeout = 2 * time.Minute

	// maxListBytes caps the model list. OpenRouter's is a few MB.
	maxListBytes = 10 << 20

	// maxReplyBytes caps every other reply.
	maxReplyBytes = 1 << 20

	// maxHostMessage caps the part of a host's error message that an
	// error passes on.
	maxHostMessage = 200

	// testMaxTokens caps the answer of a test call. Models that think
	// before they answer count those tokens too, so it is not tiny.
	testMaxTokens = 256
)

// Client calls LLM hosts. It is safe for use by many goroutines.
type Client struct {
	http *http.Client // model lists and the Test call
	chat *http.Client // the calls of an agent's answer
}

// New returns a Client. Unless allowPrivate is true, it refuses to
// connect to private addresses, such as 127.0.0.1, 10.0.0.0/8 or the
// cloud metadata address 169.254.169.254.
func New(allowPrivate bool) *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = refusePrivate
	}
	return &Client{
		http: newHTTPClient(dialer, callTimeout),
		chat: newHTTPClient(dialer, chatTimeout),
	}
}

// newHTTPClient returns an HTTP client that dials with dialer and gives
// up after timeout.
func newHTTPClient(dialer *net.Dialer, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		// No proxy from the environment: through a proxy, the dial check
		// would see the proxy's address, and the proxy could reach any
		// host it likes.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// A redirect could point at any address and take the key with
		// it, so the client stops at the first reply.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Error is a failed call. Its message is short and safe to show: it
// never holds the key or the host's raw reply.
type Error struct {
	msg string
}

func (e *Error) Error() string { return e.msg }

// Model is one entry of a host's model list.
type Model struct {
	ID string
	// ToolsKnown is true when the host lists the parameters the model
	// supports, as OpenRouter does. Other hosts do not say.
	ToolsKnown bool
	// Tools reports whether the model supports tool use. It means
	// something only when ToolsKnown is true.
	Tools bool
}

// ListModels asks the host at baseURL for its models, with GET /models.
// key may be empty for a host that needs none.
func (c *Client) ListModels(ctx context.Context, baseURL, key string) ([]Model, error) {
	data, err := c.call(ctx, c.http, http.MethodGet, baseURL+"/models", key, nil, maxListBytes)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Data *[]struct {
			ID                  string    `json:"id"`
			SupportedParameters *[]string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &reply); err != nil || reply.Data == nil {
		return nil, &Error{"the host's reply is not a model list: check the base URL"}
	}
	list := make([]Model, 0, len(*reply.Data))
	for _, d := range *reply.Data {
		m := Model{ID: d.ID}
		if d.SupportedParameters != nil {
			m.ToolsKnown = true
			for _, p := range *d.SupportedParameters {
				if p == "tools" {
					m.Tools = true
				}
			}
		}
		list = append(list, m)
	}
	return list, nil
}

// CheckModel checks that the host lists modelID. When the host says
// which models support tools, it also checks that this one does, since
// agents need tool use.
func (c *Client) CheckModel(ctx context.Context, baseURL, key, modelID string) error {
	list, err := c.ListModels(ctx, baseURL, key)
	if err != nil {
		return err
	}
	for _, m := range list {
		if m.ID != modelID {
			continue
		}
		if m.ToolsKnown && !m.Tools {
			return &Error{fmt.Sprintf("the host says model %q does not support tool use, which agents need", modelID)}
		}
		return nil
	}
	return &Error{fmt.Sprintf("the host does not list a model with the ID %q", modelID)}
}

// testTool is the one tool a test call offers.
var testTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "get_time",
		"description": "Returns the current time.",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

// Test sends the model one short message with one tool, and asks it to
// call the tool. It returns nil when the reply holds a tool call. This
// checks the base URL, the key, the model ID and tool use in one call.
func (c *Client) Test(ctx context.Context, baseURL, key, modelID string) error {
	body, err := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]string{{
			"role":    "user",
			"content": "This is a connection test. Call the get_time tool now. Do not answer in text.",
		}},
		"tools":      []any{testTool},
		"max_tokens": testMaxTokens,
	})
	if err != nil {
		return &Error{"could not build the test request"}
	}
	data, err := c.call(ctx, c.http, http.MethodPost, baseURL+"/chat/completions", key, body, maxReplyBytes)
	if err != nil {
		return err
	}
	var reply struct {
		Choices []struct {
			Message struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return &Error{"the host's reply is not a chat completion: check the base URL"}
	}
	if err := replyError(reply.Error, key); err != nil {
		return err
	}
	if len(reply.Choices) == 0 {
		return &Error{"the host's reply has no answer"}
	}
	choice := reply.Choices[0]
	if len(choice.Message.ToolCalls) > 0 {
		return nil
	}
	if choice.FinishReason == "length" {
		return &Error{"the model ran out of tokens before it called the tool"}
	}
	return &Error{"the model answered without calling the tool, so it may not support tool use"}
}

// replyError returns an *Error for the "error" value some hosts put in
// a 200 reply, or nil when the reply has none.
func replyError(raw json.RawMessage, key string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	msg := "the host answered with an error"
	if m := hostMessage(raw, key); m != "" {
		msg += ": " + m
	}
	return &Error{msg}
}

// call sends one request with hc and returns the body of a 200 reply,
// read up to limit bytes. Every error it returns is an *Error.
func (c *Client) call(ctx context.Context, hc *http.Client, method, url, key string, body []byte, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		// The base URL was checked on save, so this is rare. The message
		// leaves out err: it is about the URL, never the key, but short
		// is enough.
		return nil, &Error{"the base URL is not valid"}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "repo-speaks")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, connError(err, hc.Timeout)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, connError(err, hc.Timeout)
	}
	if int64(len(data)) > limit {
		return nil, &Error{fmt.Sprintf("the host's reply is over the %d MB limit", limit>>20)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, data, key)
	}
	return data, nil
}

// connError explains a failure to reach the host, or to read its reply.
// It never passes on err's text, which may name addresses the person
// does not need to see. timeout is the client's limit, which the message
// names when the host was too slow.
func connError(err error, timeout time.Duration) *Error {
	if pe, ok := errors.AsType[*privateError](err); ok {
		return &Error{fmt.Sprintf("the host's address %s is private, and this server blocks private addresses", pe.ip)}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{fmt.Sprintf("the host did not answer within %d seconds", int(timeout/time.Second))}
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return &Error{fmt.Sprintf("the host did not answer within %d seconds", int(timeout/time.Second))}
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return &Error{"the host name was not found"}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return &Error{"the host refused the connection"}
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return &Error{"the host's TLS certificate is not trusted"}
	}
	return &Error{"could not connect to the host"}
}

// statusError explains a reply other than 200: the status, and the
// start of the host's error message when it sent one as JSON. The
// status text is Go's, since the host's own could say anything.
func statusError(code int, data []byte, key string) *Error {
	msg := fmt.Sprintf("the host answered %d %s", code, http.StatusText(code))
	if code >= 300 && code < 400 {
		return &Error{msg + ", a redirect, which is not followed: check the base URL"}
	}
	var reply struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &reply) == nil {
		if m := hostMessage(reply.Error, key); m != "" {
			msg += ": " + m
		}
	}
	return &Error{msg}
}

// hostMessage reads a host's "error" value, an object with a message
// or a plain string. It returns the message made safe to show: the key
// is hidden, control characters become spaces, and it is cut to
// maxHostMessage characters.
func hostMessage(raw json.RawMessage, key string) string {
	var msg string
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		msg = obj.Message
	} else if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "[hidden]")
	}
	msg = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, msg)
	msg = strings.Join(strings.Fields(msg), " ")
	if utf8.RuneCountInString(msg) > maxHostMessage {
		msg = string([]rune(msg)[:maxHostMessage]) + "..."
	}
	return msg
}
