package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
	reposync "github.com/urtuba/repo-speaks/internal/sync"
)

const (
	testKey    = "sk-or-v1-0000secretvalue0000-k3y9"
	testModel  = "z-ai/glm-5.3-flash"
	testPrompt = "Answer in three bullet points. Internal note: hidden-prompt-7f3a."
	commitA    = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
	commitB    = "553c2077f0edc3d5dc5d17262f6aa498e69d6f8e"
	// secret is in a file outside every clone. No tool may return it.
	secret = "password_hash=TOPSECRET"
)

// fakeHost is an OpenAI-compatible host. reply scripts its answer to
// the nth chat call, counted from 0. It records every request body.
type fakeHost struct {
	mu       sync.Mutex
	requests []map[string]any
	reply    func(n int, req map[string]any) (status int, body string)
	running  int // calls being answered now
	maxRun   int // the most calls answered at once
	hold     chan struct{}
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	h.mu.Lock()
	n := len(h.requests)
	h.requests = append(h.requests, req)
	h.running++
	h.maxRun = max(h.maxRun, h.running)
	hold, reply := h.hold, h.reply
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.running--
		h.mu.Unlock()
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	status, body := reply(n, req)
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// script sets how the host answers, and forgets the calls so far.
func (h *fakeHost) script(reply func(n int, req map[string]any) (int, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reply, h.requests = reply, nil
}

// block makes the host hold every call until the returned channel is
// closed.
func (h *fakeHost) block() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hold = make(chan struct{})
	return h.hold
}

// calls returns the request bodies so far.
func (h *fakeHost) calls() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.requests...)
}

// toolCall is a reply that calls one tool.
func toolCall(name, args string) string {
	b, _ := json.Marshal(args)
	return `{"choices": [{"message": {"content": null, "tool_calls": [{"id": "call-` + name + `", "type": "function",
		"function": {"name": "` + name + `", "arguments": ` + string(b) + `}}]}, "finish_reason": "tool_calls"}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 10}}`
}

// textReply is a reply with an answer.
func textReply(text string) string {
	b, _ := json.Marshal(text)
	return `{"choices": [{"message": {"content": ` + string(b) + `}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 300, "completion_tokens": 50}}`
}

// replies answers the calls with replies in order, and the last one
// after that.
func replies(list ...string) func(int, map[string]any) (int, string) {
	return func(n int, _ map[string]any) (int, string) {
		return http.StatusOK, list[min(n, len(list)-1)]
	}
}

// clock is a clock tests move by hand. Answers read it from their own
// goroutines, so it has a lock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture is a project with two synced repositories (urtuba/demo and
// urtuba/docs), a model on the fake host, a chat agent that reads every
// repository and an answer agent that reads only urtuba/demo. Another
// project has a synced repository of its own, urtuba/billing. Next to
// the clones lies a fake database file with the secret.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	svc     *Service
	host    *fakeHost
	clock   *clock
	logs    *bytes.Buffer
	dataDir string
	project store.Project
	other   store.Project
	user    store.User
	user2   store.User
	demo    store.Resource
	docs    store.Resource
	billing store.Resource
	chat    store.Agent // chat agent, every repository
	answer  store.Agent // answer agent, urtuba/demo only
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{
		t: t, ctx: ctx, dataDir: t.TempDir(), logs: &bytes.Buffer{},
		clock: &clock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)},
		host:  &fakeHost{reply: replies(textReply("Nothing found."))},
	}
	var err error
	f.st, err = store.Open(ctx, filepath.Join(f.dataDir, "repo-speaks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.st.Close() })
	srv := httptest.NewServer(f.host)
	t.Cleanup(srv.Close)

	f.project = must(f.st.CreateProject(ctx, "Customer portal", f.clock.now()))
	f.other = must(f.st.CreateProject(ctx, "Billing", f.clock.now()))
	f.user = must(f.st.CreateUser(ctx, "asli", "hash", f.clock.now()))
	f.user2 = must(f.st.CreateUser(ctx, "mert", "hash", f.clock.now()))
	model := must(f.st.AddModel(ctx, store.NewModel{
		ProjectID: f.project.ID, Name: "Fast", BaseURL: srv.URL + "/v1", ModelID: testModel, APIKey: testKey,
	}, f.clock.now()))
	if err := f.st.SetDefaultModel(ctx, f.project.ID, model.ID); err != nil {
		t.Fatal(err)
	}

	f.demo = f.addRepo(f.project.ID, "urtuba", "demo", commitA, map[string]string{
		"README.md":           "# Demo\nA demo shop.\nRefunds are described in app/refund.go.\n",
		"app/refund.go":       "package app\n\n// Refund pays a client back within 5 days.\nfunc Refund() {}\n",
		"app/cancel.go":       "package app\n\n// Clients cannot CANCEL after 24 hours.\nconst cancelHours = 24\n",
		"docs/guide.md":       "Ask support to cancel an order.\n",
		"assets/logo.png":     "\x89PNG\r\n\x1a\n\x00\x00\x00 cancel",
		"data/latin1.txt":     "caf\xe9 cancel\n",
		"app/deep/a/b/c.go":   "package c\n",
		"vendor/big/many.txt": strings.Repeat("cancel line\n", 120),
	})
	f.docs = f.addRepo(f.project.ID, "urtuba", "docs", commitB, map[string]string{
		"README.md":  "# Docs\n",
		"how-to.md":  "To cancel, write to support.\n",
		"secret.txt": "docs-only-text\n",
	})
	f.billing = f.addRepo(f.other.ID, "urtuba", "billing", commitB, map[string]string{
		"README.md":  "# Billing\n",
		"invoice.go": "package billing // billing-only-text\n",
	})
	// The fake database file next to the clones.
	if err := os.WriteFile(filepath.Join(f.dataDir, "fake-database.db"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.chat = must(f.st.AddAgent(ctx, f.project.ID, store.AgentChat, store.AgentSettings{
		Name: "Helper", Prompt: testPrompt, RateLimitPerHour: 20, AllResources: true,
	}, f.clock.now()))
	f.answer = must(f.st.AddAgent(ctx, f.project.ID, store.AgentAnswer, store.AgentSettings{
		Name: "Triage", Prompt: "Answer yes or no.", RateLimitPerHour: 20, ResourceIDs: []int64{f.demo.ID},
	}, f.clock.now()))

	f.svc = newService(f.st, llm.New(true), f.dataDir, slog.New(slog.NewTextHandler(f.logs, nil)), f.clock.now)
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
		f.svc.Wait()
	})
	if err := f.svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// must returns v, and panics on err, which fails the test. It is for
// setup steps that never fail.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// addRepo adds a repository to a project and writes its clone, as a
// sync would.
func (f *fixture) addRepo(projectID int64, owner, name, commit string, files map[string]string) store.Resource {
	f.t.Helper()
	r := must(f.st.AddResource(f.ctx, store.NewResource{ProjectID: projectID, Owner: owner, Repo: name}, f.clock.now()))
	dir := reposync.ClonePath(f.dataDir, projectID, r.ID, "1760000000")
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.st.SaveSync(f.ctx, r.ID, "1760000000", commit, f.clock.now()); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// cloneDir is the folder of a repository's clone.
func (f *fixture) cloneDir(r store.Resource) string {
	return reposync.ClonePath(f.dataDir, r.ProjectID, r.ID, "1760000000")
}

// ask asks a question as asli, waits for the answer, and returns the
// chat's messages.
func (f *fixture) ask(a store.Agent, chatID int64, question string) (int64, []store.Message) {
	f.t.Helper()
	id, err := f.svc.Ask(f.ctx, Question{ProjectID: f.project.ID, AgentID: a.ID, UserID: f.user.ID, ChatID: chatID, Text: question})
	if err != nil {
		f.t.Fatal(err)
	}
	f.svc.Wait()
	return id, must(f.st.Messages(f.ctx, f.project.ID, id))
}

// last returns the last message.
func last(list []store.Message) store.Message {
	return list[len(list)-1]
}

// messagesOf returns the "messages" of a request as role and content.
func messagesOf(req map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := req["messages"].([]any)
	for _, m := range list {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestAnswerEndToEnd(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(
		toolCall("search_text", `{"query": "refund"}`),
		toolCall("read_file", `{"path": "urtuba/demo/app/refund.go", "from_line": 1, "to_line": 4}`),
		textReply("Clients get their money back within 5 days (urtuba/demo/app/refund.go:3-4). "+
			"The README says so too: urtuba/demo/README.md:3. "+
			"A guess about urtuba/demo/app/cancel.go:3 is dropped, since it was not read."),
	))
	_, list := f.ask(f.chat, 0, "How long do refunds take?")
	a := last(list)
	if a.Status != store.AnswerDone || !strings.HasPrefix(a.Content, "Clients get their money back") {
		t.Fatalf("answer = %+v", a)
	}
	wantUsage := store.AnswerUsage{ModelID: testModel, PromptTokens: 500, CompletionTokens: 70, Steps: 2}
	if a.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", a.Usage, wantUsage)
	}
	wantSteps := []string{"Searching for refund", "Reading urtuba/demo/app/refund.go"}
	if fmt.Sprint(a.Progress) != fmt.Sprint(wantSteps) {
		t.Errorf("Progress = %q, want %q", a.Progress, wantSteps)
	}
	want := []store.Source{
		{Position: 1, ResourceID: f.demo.ID, Owner: "urtuba", Repo: "demo", Path: "app/refund.go", LineFrom: 3, LineTo: 4, CommitID: commitA},
		{Position: 2, ResourceID: f.demo.ID, Owner: "urtuba", Repo: "demo", Path: "README.md", LineFrom: 3, LineTo: 3, CommitID: commitA},
	}
	if fmt.Sprint(a.Sources) != fmt.Sprint(want) {
		t.Errorf("Sources = %+v\nwant %+v", a.Sources, want)
	}

	calls := f.host.calls()
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3", len(calls))
	}
	first := messagesOf(calls[0])
	if len(first) != 3 {
		t.Fatalf("first call has %d messages, want system, overview, question", len(first))
	}
	system, overview, question := first[0]["content"].(string), first[1]["content"].(string), first[2]["content"].(string)
	if !strings.HasPrefix(system, baseRules) || !strings.HasSuffix(system, testPrompt) {
		t.Errorf("system message = %q, want the base rules, then the instructions", system)
	}
	for _, want := range []string{
		"## urtuba/demo (commit 7fd1a60)", "## urtuba/docs (commit 553c207)", "app/refund.go\n",
		"README: urtuba/demo/README.md, lines 1 to 3 of 3:\n1: # Demo", "how-to.md\n",
	} {
		if !strings.Contains(overview, want) {
			t.Errorf("overview does not contain %q:\n%s", want, overview)
		}
	}
	if strings.Contains(overview, "billing") {
		t.Errorf("overview names another project's repository:\n%s", overview)
	}
	if question != "How long do refunds take?" {
		t.Errorf("question = %q", question)
	}
	if calls[0]["model"] != testModel || len(calls[0]["tools"].([]any)) != 3 || calls[0]["tool_choice"] != nil {
		t.Errorf("first call: model %v, tools %v, tool_choice %v", calls[0]["model"], calls[0]["tools"], calls[0]["tool_choice"])
	}
	third := messagesOf(calls[2])
	searchResult := third[4]["content"].(string)
	readResult := third[6]["content"].(string)
	if third[4]["role"] != "tool" || third[4]["tool_call_id"] != "call-search_text" ||
		!strings.Contains(searchResult, "urtuba/demo/app/refund.go:3: // Refund pays a client back within 5 days.") {
		t.Errorf("search result = %v", third[4])
	}
	if !strings.Contains(readResult, "urtuba/demo/app/refund.go, lines 1 to 4 of 4:\n1: package app\n") {
		t.Errorf("read result = %q", readResult)
	}
}

func TestFollowUpGetsHistoryAsText(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(toolCall("list_files", `{"path": "urtuba/demo"}`), textReply("First answer.")))
	chatID, _ := f.ask(f.chat, 0, "First question?")

	f.host.script(replies(textReply("Second answer.")))
	_, list := f.ask(f.chat, chatID, "Second question?")
	if len(list) != 4 || last(list).Content != "Second answer." {
		t.Fatalf("messages = %+v", list)
	}
	msgs := messagesOf(f.host.calls()[0])
	var got []string
	for _, m := range msgs[2:] {
		got = append(got, fmt.Sprint(m["role"], ": ", m["content"]))
	}
	want := []string{"user: First question?", "assistant: First answer.", "user: Second question?"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("messages after the overview = %q, want %q (no old tool calls)", got, want)
	}
}

func TestFailedAnswersLeaveHistory(t *testing.T) {
	f := newFixture(t)
	f.host.script(func(int, map[string]any) (int, string) { return http.StatusInternalServerError, `{}` })
	chatID, list := f.ask(f.chat, 0, "Broken question?")
	if last(list).Status != store.AnswerFailed {
		t.Fatalf("answer = %+v, want failed", last(list))
	}
	f.host.script(replies(textReply("Fine.")))
	f.ask(f.chat, chatID, "Next question?")
	msgs := messagesOf(f.host.calls()[0])
	if len(msgs) != 3 || msgs[2]["content"] != "Next question?" {
		t.Errorf("messages = %v, want no failed pair", msgs)
	}
}

func TestStepLimit(t *testing.T) {
	f := newFixture(t)
	// The model calls two tools at a time and never stops, until it is
	// told to answer.
	f.host.script(func(n int, req map[string]any) (int, string) {
		if req["tool_choice"] == "none" {
			return http.StatusOK, textReply("Here is what I found.")
		}
		return http.StatusOK, `{"choices": [{"message": {"tool_calls": [
			{"id": "a` + fmt.Sprint(n) + `", "type": "function", "function": {"name": "list_files", "arguments": "{\"path\": \"urtuba/demo\"}"}},
			{"id": "b` + fmt.Sprint(n) + `", "type": "function", "function": {"name": "list_files", "arguments": "{\"path\": \"urtuba/docs\"}"}}
		]}, "finish_reason": "tool_calls"}]}`
	})
	_, list := f.ask(f.chat, 0, "Look around?")
	a := last(list)
	if a.Status != store.AnswerDone || a.Content != "Here is what I found." || a.Usage.Steps != maxSteps {
		t.Fatalf("answer = %+v, want done after %d steps", a, maxSteps)
	}
	calls := f.host.calls()
	if len(calls) != maxSteps/2+1 {
		t.Errorf("got %d calls, want %d", len(calls), maxSteps/2+1)
	}
	final := calls[len(calls)-1]
	msgs := messagesOf(final)
	if final["tool_choice"] != "none" || !strings.Contains(msgs[len(msgs)-1]["content"].(string), "You have used all 30 tool steps") {
		t.Errorf("last call: tool_choice %v, last message %v", final["tool_choice"], msgs[len(msgs)-1])
	}
	if got := lastString(a.Progress); got != "Writing the answer from what was found" {
		t.Errorf("last step = %q", got)
	}
}

// lastString returns the last string.
func lastString(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[len(list)-1]
}

func TestStepLimitMidReply(t *testing.T) {
	f := newFixture(t)
	f.svc.maxSteps = 3
	f.host.script(func(n int, req map[string]any) (int, string) {
		if req["tool_choice"] == "none" {
			return http.StatusOK, textReply("Done.")
		}
		return http.StatusOK, `{"choices": [{"message": {"tool_calls": [
			{"id": "a", "type": "function", "function": {"name": "list_files", "arguments": "{\"path\": \"urtuba/demo\"}"}},
			{"id": "b", "type": "function", "function": {"name": "list_files", "arguments": "{\"path\": \"urtuba/demo/app\"}"}}
		]}, "finish_reason": "tool_calls"}]}`
	})
	_, list := f.ask(f.chat, 0, "Look around?")
	if a := last(list); a.Usage.Steps != 3 || a.Status != store.AnswerDone {
		t.Fatalf("answer = %+v, want done after 3 steps", a)
	}
	// The fourth call was not run, but it got a result.
	msgs := messagesOf(f.host.calls()[2])
	if got := msgs[len(msgs)-2]["content"]; got != "Not run: you have used all 3 tool steps." {
		t.Errorf("result of the call past the limit = %v", got)
	}
}

func TestTimeLimit(t *testing.T) {
	f := newFixture(t)
	f.svc.timeLimit = 300 * time.Millisecond
	f.host.block() // never answers
	_, list := f.ask(f.chat, 0, "Slow?")
	a := last(list)
	if a.Status != store.AnswerFailed || !strings.HasPrefix(a.Error, "The answer took longer than") {
		t.Errorf("answer = %+v", a)
	}
	for d, want := range map[time.Duration]string{
		5 * time.Minute: "5 minutes", time.Minute: "1 minute", 30 * time.Second: "30 seconds",
	} {
		if got := minutes(d); got != want {
			t.Errorf("minutes(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestHostFailure(t *testing.T) {
	f := newFixture(t)
	f.host.script(func(int, map[string]any) (int, string) {
		return http.StatusUnauthorized, `{"error": {"message": "Key ` + testKey + ` is not valid"}}`
	})
	_, list := f.ask(f.chat, 0, "Hello?")
	a := last(list)
	want := "The model's host failed: the host answered 401 Unauthorized: Key [hidden] is not valid."
	if a.Status != store.AnswerFailed || a.Error != want || a.Usage.ModelID != testModel {
		t.Errorf("answer = %+v\nwant error %q", a, want)
	}
	// A host that cannot be reached.
	if err := f.st.SetDefaultModel(f.ctx, f.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	m := must(f.st.AddModel(f.ctx, store.NewModel{
		ProjectID: f.project.ID, Name: "Down", BaseURL: "http://127.0.0.1:1/v1", ModelID: testModel, APIKey: testKey,
	}, f.clock.now()))
	if err := f.st.SetDefaultModel(f.ctx, f.project.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	_, list = f.ask(f.chat, 0, "Hello again?")
	if a := last(list); a.Error != "The model's host failed: the host refused the connection." {
		t.Errorf("answer = %+v", a)
	}
	if strings.Contains(f.logs.String(), "secretvalue") {
		t.Errorf("the log holds the key:\n%s", f.logs)
	}
}

func TestEmptyAnswer(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(`{"choices": [{"message": {"content": ""}, "finish_reason": "length"}]}`))
	_, list := f.ask(f.chat, 0, "Hello?")
	if a := last(list); a.Status != store.AnswerFailed || a.Error != "The model ran out of tokens before it answered." {
		t.Errorf("answer = %+v", a)
	}
}

func TestStartMarksRunningAnswersFailed(t *testing.T) {
	f := newFixture(t)
	chatID, _, err := f.st.StartChat(f.ctx, f.project.ID, f.chat.ID, f.user.ID, "Cut off?", f.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	svc := newService(f.st, llm.New(true), f.dataDir, slog.New(slog.NewTextHandler(io.Discard, nil)), f.clock.now)
	if err := svc.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	a := last(must(f.st.Messages(f.ctx, f.project.ID, chatID)))
	if a.Status != store.AnswerFailed || !strings.Contains(a.Error, "The server restarted") {
		t.Errorf("answer = %+v", a)
	}
}

func TestServerStopFailsAnswers(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	svc := newService(f.st, llm.New(true), f.dataDir, slog.New(slog.NewTextHandler(io.Discard, nil)), f.clock.now)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f.host.block()
	chatID, err := svc.Ask(f.ctx, Question{ProjectID: f.project.ID, AgentID: f.chat.ID, UserID: f.user.ID, Text: "Hi?"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.host.calls()) == 1 })
	cancel()
	svc.Wait()
	a := last(must(f.st.Messages(f.ctx, f.project.ID, chatID)))
	if a.Status != store.AnswerFailed || a.Error != msgStopped {
		t.Errorf("answer = %+v", a)
	}
}

// waitFor waits up to 5 seconds for ok to hold.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gave up waiting")
}

func TestParallelLimit(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(textReply("Done.")))
	hold := f.host.block()
	var chats []int64
	for range parallel + 2 {
		id, err := f.svc.Ask(f.ctx, Question{ProjectID: f.project.ID, AgentID: f.chat.ID, UserID: f.user.ID, Text: "Hi?"})
		if err != nil {
			t.Fatal(err)
		}
		chats = append(chats, id)
	}
	waitFor(t, func() bool { return len(f.host.calls()) == parallel })
	time.Sleep(50 * time.Millisecond) // give a fifth answer the chance to start, if it could
	if n := len(f.host.calls()); n != parallel {
		t.Errorf("%d answers ran at once, want %d", n, parallel)
	}
	close(hold)
	f.svc.Wait()
	for _, id := range chats {
		if a := last(must(f.st.Messages(f.ctx, f.project.ID, id))); a.Status != store.AnswerDone {
			t.Errorf("chat %d: answer = %+v", id, a)
		}
	}
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	if f.host.maxRun != parallel {
		t.Errorf("at most %d calls ran at once, want %d", f.host.maxRun, parallel)
	}
}

func TestAskRefusals(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(textReply("Yes.")))
	answerChat, _ := f.ask(f.answer, 0, "Can clients cancel?")
	helperChat, _ := f.ask(f.chat, 0, "Hello?")

	f.svc.limits = newLimiter()

	tests := []struct {
		name string
		q    Question
		want error
	}{
		{"second question to an answer agent", Question{AgentID: f.answer.ID, ChatID: answerChat}, ErrOneQuestion},
		{"another member's chat", Question{AgentID: f.chat.ID, ChatID: helperChat, UserID: f.user2.ID}, store.ErrNotFound},
		{"the chat of another agent", Question{AgentID: f.answer.ID, ChatID: helperChat}, store.ErrNotFound},
		{"a missing chat", Question{AgentID: f.chat.ID, ChatID: 999}, store.ErrNotFound},
		{"another project's agent", Question{ProjectID: f.other.ID, AgentID: f.chat.ID}, store.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := tt.q
			if q.ProjectID == 0 {
				q.ProjectID = f.project.ID
			}
			if q.UserID == 0 {
				q.UserID = f.user.ID
			}
			q.Text = "Again?"
			if _, err := f.svc.Ask(f.ctx, q); !errors.Is(err, tt.want) {
				t.Errorf("Ask() error = %v, want %v", err, tt.want)
			}
		})
	}
	// An agent with no model of its own, when the project has no
	// default, is not ready.
	if err := f.st.SetDefaultModel(f.ctx, f.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	q := Question{ProjectID: f.project.ID, AgentID: f.chat.ID, UserID: f.user.ID, Text: "Hi?"}
	if _, err := f.svc.Ask(f.ctx, q); !errors.Is(err, ErrNotReady) {
		t.Errorf("Ask(not ready) error = %v, want ErrNotReady", err)
	}
	// None of these counted against the rate limit, and Ask made no
	// messages.
	if len(f.svc.limits.asked) != 0 {
		t.Errorf("refused questions were counted: %v", f.svc.limits.asked)
	}
	if n := len(must(f.st.Messages(f.ctx, f.project.ID, answerChat))); n != 2 {
		t.Errorf("answer chat has %d messages, want 2", n)
	}
}

func TestAskWhileRunning(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(textReply("Done.")))
	hold := f.host.block()
	q := Question{ProjectID: f.project.ID, AgentID: f.chat.ID, UserID: f.user.ID, Text: "First?"}
	chatID, err := f.svc.Ask(f.ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.ChatID, q.Text = chatID, "Second?"
	if _, err := f.svc.Ask(f.ctx, q); !errors.Is(err, store.ErrAnswerRunning) {
		t.Errorf("Ask() while running: error = %v, want ErrAnswerRunning", err)
	}
	close(hold)
	f.svc.Wait()
	// The refused question was given back to the rate limit.
	if n := len(f.svc.limits.asked[limitKey{f.chat.ID, f.user.ID}]); n != 1 {
		t.Errorf("rate limit counts %d questions, want 1", n)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(textReply("Done.")))
	set := store.AgentSettings{Name: "Helper", Prompt: testPrompt, RateLimitPerHour: 2, AllResources: true}
	if err := f.st.UpdateAgent(f.ctx, f.project.ID, f.chat.ID, set, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	ask := func(user, agent int64) error {
		_, err := f.svc.Ask(f.ctx, Question{ProjectID: f.project.ID, AgentID: agent, UserID: user, Text: "Hi?"})
		f.svc.Wait()
		return err
	}
	first := f.clock.now()
	if err := ask(f.user.ID, f.chat.ID); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(10 * time.Minute)
	if err := ask(f.user.ID, f.chat.ID); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(10 * time.Minute)
	err := ask(f.user.ID, f.chat.ID)
	rle, ok := errors.AsType[*RateLimitError](err)
	if !ok || !rle.RetryAt.Equal(first.Add(time.Hour)) || rle.Limit != 2 {
		t.Fatalf("third question: error = %v, want a rate limit until %v", err, first.Add(time.Hour))
	}
	if want := "You can ask this agent 2 questions per hour. You can ask again at 13:00 UTC."; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	// Other members and other agents have their own counts.
	if err := ask(f.user2.ID, f.chat.ID); err != nil {
		t.Errorf("another member: %v", err)
	}
	if err := ask(f.user.ID, f.answer.ID); err != nil {
		t.Errorf("another agent: %v", err)
	}
	// The window slides: an hour after the first question, one more.
	f.clock.advance(40 * time.Minute)
	if err := ask(f.user.ID, f.chat.ID); err != nil {
		t.Errorf("after the window: %v", err)
	}
	if err := ask(f.user.ID, f.chat.ID); err == nil {
		t.Error("a fourth question within the window passed")
	}
}

func TestNotSyncedRepository(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.AddResource(f.ctx, store.NewResource{ProjectID: f.project.ID, Owner: "urtuba", Repo: "fresh"}, f.clock.now()); err != nil {
		t.Fatal(err)
	}
	f.ask(f.chat, 0, "Hello?")
	overview := messagesOf(f.host.calls()[0])[1]["content"].(string)
	if !strings.Contains(overview, "These repositories are not synced yet, so you cannot read them: urtuba/fresh.") {
		t.Errorf("overview does not name the repository never synced:\n%s", overview)
	}
}

func TestOnlyPickedAndOwnRepositories(t *testing.T) {
	f := newFixture(t)
	f.host.script(replies(
		toolCall("read_file", `{"path": "urtuba/docs/secret.txt"}`),
		toolCall("read_file", `{"path": "urtuba/billing/invoice.go"}`),
		toolCall("search_text", `{"query": "only-text"}`),
		textReply("Nothing."),
	))
	// The answer agent reads urtuba/demo only.
	_, list := f.ask(f.answer, 0, "What is in the other repositories?")
	if a := last(list); a.Status != store.AnswerDone {
		t.Fatalf("answer = %+v", a)
	}
	msgs := messagesOf(f.host.calls()[3])
	overview := msgs[1]["content"].(string)
	if strings.Contains(overview, "urtuba/docs") || strings.Contains(overview, "billing") {
		t.Errorf("overview names a repository the agent does not read:\n%s", overview)
	}
	for i, want := range []string{
		`Error: "urtuba/docs/secret.txt" is in none of your repositories. Paths start with one of: urtuba/demo.`,
		`Error: "urtuba/billing/invoice.go" is in none of your repositories. Paths start with one of: urtuba/demo.`,
		`No lines contain "only-text".`,
	} {
		if got := msgs[3+2*i+1]["content"]; got != want {
			t.Errorf("result %d = %q, want %q", i, got, want)
		}
	}
}
