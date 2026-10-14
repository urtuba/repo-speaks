package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
	reposync "github.com/urtuba/repo-speaks/internal/sync"
)

const (
	demoCommit   = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
	answerPrompt = "Answer yes or no. Secret format note: answer-prompt-9c2e."
)

// chatHost is an OpenAI-compatible host for answers. It answers the
// calls with replies in order, then the last one again. While hold is
// open, every call waits.
type chatHost struct {
	mu      sync.Mutex
	replies []string
	status  int
	calls   int
	hold    chan struct{}
}

func (h *chatHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	h.mu.Lock()
	n := h.calls
	h.calls++
	hold, status, replies := h.hold, h.status, h.replies
	h.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(status)
	io.WriteString(w, replies[min(n, len(replies)-1)])
}

// script sets the replies, with status 200 unless status is given.
func (h *chatHost) script(replies ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replies, h.status, h.calls = replies, http.StatusOK, 0
}

// block makes every call wait until the returned channel is closed.
func (h *chatHost) block() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hold = make(chan struct{})
	return h.hold
}

// unblock lets calls through again.
func (h *chatHost) unblock(hold chan struct{}) {
	h.mu.Lock()
	h.hold = nil
	h.mu.Unlock()
	close(hold)
}

// toolCallJSON is a reply that calls one tool.
func toolCallJSON(name string, args map[string]any) string {
	a, _ := json.Marshal(args)
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": string(a)},
		}}},
		"finish_reason": "tool_calls",
	}}})
	return string(b)
}

// answerJSON is a reply with an answer.
func answerJSON(text string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"content": text}, "finish_reason": "stop",
	}}})
	return string(b)
}

// chatApp is the agent fixture (see agentApp) where the chat agent
// Helper is ready: the project's default model is on a fake host. The
// project also has Triage, an answer agent with its own instructions.
// urtuba/demo has a clone with app/refund.go.
type chatApp struct {
	agentApp
	host   *chatHost
	triage store.Agent
}

func newChatApp(t *testing.T) chatApp {
	t.Helper()
	ctx := context.Background()
	app := chatApp{agentApp: newAgentApp(t), host: &chatHost{}}
	app.host.script(answerJSON("Nothing found."))
	srv := httptest.NewServer(app.host)
	t.Cleanup(srv.Close)
	app.useLLM(llm.New(true))

	m, err := app.store.AddModel(ctx, store.NewModel{
		ProjectID: app.project.ID, Name: "Local", BaseURL: srv.URL + "/v1", ModelID: glm, APIKey: modelKey,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.SetDefaultModel(ctx, app.project.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	app.triage, err = app.store.AddAgent(ctx, app.project.ID, store.AgentAnswer, store.AgentSettings{
		Name: "Triage", Description: "Says yes or no.", Prompt: answerPrompt, RateLimitPerHour: 20, AllResources: true,
	}, app.clock.now())
	if err != nil {
		t.Fatal(err)
	}

	dir := reposync.ClonePath(app.dataDir, app.project.ID, app.res.ID, "1760000000")
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	refund := "package app\n\n// Refund pays a client back within 5 days.\nfunc Refund() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "app", "refund.go"), []byte(refund), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.store.SaveSync(ctx, app.res.ID, "1760000000", demoCommit, app.clock.now()); err != nil {
		t.Fatal(err)
	}
	return app
}

// ask posts a first question to the agent as username.
func (a chatApp) ask(username string, agentID int64, question string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.post(askPath(a.project.ID, agentID), url.Values{"question": {question}}, a.login(username))
}

// startChat asks a first question as username, waits for the answer,
// and returns the chat's path.
func (a chatApp) startChat(username string, agentID int64, question string) string {
	a.t.Helper()
	rec := a.ask(username, agentID, question)
	if rec.Code != http.StatusSeeOther {
		a.t.Fatalf("ask: status = %d, want 303\n%s", rec.Code, rec.Body)
	}
	a.answers.Wait()
	return rec.Header().Get("Location")
}

// noPrompt fails the test when body holds an agent's instructions.
func noPrompt(t *testing.T, body string) {
	t.Helper()
	for _, p := range []string{"hidden-prompt-7f3a", "answer-prompt-9c2e", "billing-prompt"} {
		if strings.Contains(body, p) {
			t.Errorf("page shows an agent's instructions (%s)", p)
		}
	}
}

func TestProjectPageAgentLinks(t *testing.T) {
	app := newChatApp(t)
	path := projectPath(app.project.ID)
	helper := `href="` + askPath(app.project.ID, app.agent.ID) + `">Chat</a>`
	triage := `href="` + askPath(app.project.ID, app.triage.ID) + `">Ask</a>`
	allChats := `href="` + path + `/chats">All chats</a>`

	for _, name := range []string{"nur", "asli", "bora", "mert"} {
		body := app.get(path, app.login(name)).Body.String()
		checkBody(t, body, []string{helper, triage}, nil)
		noPrompt(t, body)
		if got, want := strings.Contains(body, allChats), name == "mert"; got != want {
			t.Errorf("%s: All chats link shown = %v, want %v", name, got, want)
		}
	}

	// A not-ready agent keeps its badge and gets no link.
	if err := app.store.SetDefaultModel(context.Background(), app.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	body := app.get(path, app.login("asli")).Body.String()
	checkBody(t, body, []string{">not ready</span>"}, []string{helper, triage})
}

func TestChatEndToEnd(t *testing.T) {
	app := newChatApp(t)
	app.host.script(
		toolCallJSON("read_file", map[string]any{"path": "urtuba/demo/app/refund.go"}),
		answerJSON("Clients get their money back **within 5 days** (urtuba/demo/app/refund.go:3-4)."),
	)
	hold := app.host.block()
	asli := app.login("asli")
	rec := app.post(askPath(app.project.ID, app.agent.ID), url.Values{"question": {"How long do refunds take?"}}, asli)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ask: status = %d, want 303\n%s", rec.Code, rec.Body)
	}
	chat := rec.Header().Get("Location")
	if !strings.HasPrefix(chat, projectPath(app.project.ID)+"/chats/") {
		t.Fatalf("Location = %q", chat)
	}

	// While the answer runs, the body reloads itself and the box is off.
	body := app.get(chat, asli).Body.String()
	checkBody(t, body, []string{
		"How long do refunds take?",
		`hx-get="` + chat + `/messages" hx-trigger="every 2s" hx-swap="outerHTML"`,
		"Waiting to start",
		"required disabled>",
		"Members with chats:view, such as managers, can read this chat.",
	}, nil)
	noPrompt(t, body)

	app.host.unblock(hold)
	app.answers.Wait()

	for _, path := range []string{chat, chat + "/messages"} {
		body = app.get(path, asli).Body.String()
		checkBody(t, body, []string{
			"Clients get their money back <strong>within 5 days</strong>",
			`<a href="https://github.com/urtuba/demo/blob/` + demoCommit + `/app/refund.go#L3-L4" rel="noreferrer">urtuba/demo/app/refund.go</a>`,
			"lines 3-4",
			`<ol class="sources">`,
		}, []string{"hx-trigger", "disabled", "Waiting to start"})
		noPrompt(t, body)
	}
	if strings.Contains(body, "<!doctype") {
		t.Error("the fragment is a whole page")
	}

	// A follow-up question.
	app.host.script(answerJSON("Yes, every client."))
	rec = app.post(chat, url.Values{"question": {"Every client?"}}, asli)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != chat {
		t.Fatalf("follow-up: status = %d, Location = %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	app.answers.Wait()
	checkBody(t, app.get(chat, asli).Body.String(), []string{"Every client?", "Yes, every client."}, nil)

	// The chat is listed on the agent's page.
	checkBody(t, app.get(askPath(app.project.ID, app.agent.ID), asli).Body.String(),
		[]string{`<a href="` + chat + `">How long do refunds take?</a>`}, nil)
}

func TestChatAccess(t *testing.T) {
	app := newChatApp(t)
	chat := app.startChat("asli", app.agent.ID, "How do refunds work?")
	ctx := context.Background()
	owner, err := app.store.UserByUsername(ctx, "samed")
	if err != nil {
		t.Fatal(err)
	}
	billingChat, _, err := app.store.StartChat(ctx, app.other.ProjectID, app.otherAgent.ID, owner.ID, "Billing?", app.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	otherHere := chatPath(app.project.ID, billingChat)
	otherThere := chatPath(app.other.ProjectID, billingChat)
	all := projectPath(app.project.ID) + "/chats"

	tests := []struct {
		name     string
		username string
		path     string
		want     int
	}{
		{"the member who started it", "asli", chat, http.StatusOK},
		{"the member who started it, fragment", "asli", chat + "/messages", http.StatusOK},
		{"a manager with chats:view", "mert", chat, http.StatusOK},
		{"an admin with chats:view", "ada", chat, http.StatusOK},
		{"the owner", "samed", chat, http.StatusOK},
		{"a builder without chats:view", "bora", chat, http.StatusNotFound},
		{"a builder, fragment", "bora", chat + "/messages", http.StatusNotFound},
		{"a member with no permissions", "nur", chat, http.StatusNotFound},
		{"an outsider", "otto", chat, http.StatusNotFound},
		{"another project's chat by ID", "mert", otherHere, http.StatusNotFound},
		{"another project's chat by ID, owner", "samed", otherHere, http.StatusNotFound},
		{"another project's URL", "mert", otherThere, http.StatusNotFound},
		{"a missing chat", "asli", chatPath(app.project.ID, 999), http.StatusNotFound},
		{"a bad ID", "asli", projectPath(app.project.ID) + "/chats/abc", http.StatusNotFound},
		{"all chats, manager", "mert", all, http.StatusOK},
		{"all chats, asker", "asli", all, http.StatusForbidden},
		{"all chats, outsider", "otto", all, http.StatusNotFound},
		{"the ask page, member with no permissions", "nur", askPath(app.project.ID, app.agent.ID), http.StatusOK},
		{"the ask page, outsider", "otto", askPath(app.project.ID, app.agent.ID), http.StatusNotFound},
		{"the ask page of another project's agent", "asli", askPath(app.project.ID, app.otherAgent.ID), http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := app.get(tt.path, app.login(tt.username))
			if rec.Code != tt.want {
				t.Errorf("GET %s as %s: status = %d, want %d", tt.path, tt.username, rec.Code, tt.want)
			}
			noPrompt(t, rec.Body.String())
		})
	}

	// Only the member who started a chat asks in it.
	posts := []struct {
		username string
		want     int
	}{{"mert", http.StatusForbidden}, {"bora", http.StatusNotFound}, {"otto", http.StatusNotFound}}
	for _, tt := range posts {
		rec := app.post(chat, url.Values{"question": {"Mine now?"}}, app.login(tt.username))
		if rec.Code != tt.want {
			t.Errorf("POST %s as %s: status = %d, want %d", chat, tt.username, rec.Code, tt.want)
		}
	}
	if rec := app.ask("otto", app.agent.ID, "Can I ask?"); rec.Code != http.StatusNotFound {
		t.Errorf("an outsider asks: status = %d, want 404", rec.Code)
	}
	// A reader with chats:view gets no question box.
	body := app.get(chat, app.login("mert")).Body.String()
	checkBody(t, body, []string{"Started by asli"}, []string{`name="question"`})

	// The All chats page lists every chat of the project, and no other.
	body = app.get(all, app.login("mert")).Body.String()
	checkBody(t, body, []string{"How do refunds work?", "Helper", "asli"}, []string{"Billing?"})
}

func TestAnswerAgentTakesOneQuestion(t *testing.T) {
	app := newChatApp(t)
	app.host.script(answerJSON("Yes."))
	asli := app.login("asli")
	body := app.get(askPath(app.project.ID, app.triage.ID), asli).Body.String()
	checkBody(t, body, []string{"Ask a question", "Says yes or no.", "one answer per question"}, nil)
	noPrompt(t, body)

	chat := app.startChat("asli", app.triage.ID, "Can clients cancel?")
	body = app.get(chat, asli).Body.String()
	checkBody(t, body, []string{"Yes.", "This agent answers one question per chat.",
		`<a href="` + askPath(app.project.ID, app.triage.ID) + `">Ask a new question</a>`}, []string{`name="question"`})
	noPrompt(t, body)

	rec := app.post(chat, url.Values{"question": {"And after 24 hours?"}}, asli)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "This agent answers one question per chat.") {
		t.Errorf("second question: status = %d\n%s", rec.Code, rec.Body)
	}
	list, err := app.store.Messages(context.Background(), app.project.ID, chatIDOf(t, chat))
	if err != nil || len(list) != 2 {
		t.Errorf("messages = %d, %v, want 2", len(list), err)
	}
}

// chatIDOf reads the chat's ID from its path.
func chatIDOf(t *testing.T, path string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAskRefusals(t *testing.T) {
	app := newChatApp(t)
	ctx := context.Background()

	// An empty or long question.
	for _, q := range []string{"  ", strings.Repeat("a", maxQuestion+1)} {
		if rec := app.ask("asli", app.agent.ID, q); rec.Code != http.StatusBadRequest {
			t.Errorf("question of %d characters: status = %d, want 400", len(q), rec.Code)
		}
	}

	// The rate limit.
	set := store.AgentSettings{Name: "Helper", Prompt: agentPrompt, RateLimitPerHour: 1, AllResources: true}
	if err := app.store.UpdateAgent(ctx, app.project.ID, app.agent.ID, set, app.clock.now()); err != nil {
		t.Fatal(err)
	}
	chat := app.startChat("asli", app.agent.ID, "First?")
	rec := app.ask("asli", app.agent.ID, "Second?")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "You can ask this agent 1 question per hour. You can ask again at ") {
		t.Errorf("over the rate limit: status = %d\n%s", rec.Code, rec.Body)
	}
	checkBody(t, rec.Body.String(), []string{">Second?</textarea>"}, nil)
	rec = app.post(chat, url.Values{"question": {"Follow-up?"}}, app.login("asli"))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("follow-up over the rate limit: status = %d", rec.Code)
	}
	// Another member has their own count.
	app.startChat("nur", app.agent.ID, "Mine?")

	// A not-ready agent.
	if err := app.store.SetDefaultModel(ctx, app.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	checkBody(t, app.get(askPath(app.project.ID, app.triage.ID), app.login("asli")).Body.String(),
		[]string{"This agent is not ready"}, []string{`name="question"`})
	if rec := app.ask("asli", app.triage.ID, "Hello?"); rec.Code != http.StatusConflict {
		t.Errorf("not-ready agent: status = %d, want 409", rec.Code)
	}
}

func TestDeletedAgentAndAccount(t *testing.T) {
	app := newChatApp(t)
	ctx := context.Background()
	chat := app.startChat("asli", app.agent.ID, "Who am I?")
	if err := app.store.DeleteAgent(ctx, app.project.ID, app.agent.ID); err != nil {
		t.Fatal(err)
	}
	body := app.get(chat, app.login("asli")).Body.String()
	checkBody(t, body, []string{"Agent: deleted agent.", "The agent was deleted, so this chat takes no new questions."},
		[]string{`name="question"`})
	if rec := app.post(chat, url.Values{"question": {"Still there?"}}, app.login("asli")); rec.Code != http.StatusConflict {
		t.Errorf("question to a deleted agent: status = %d, want 409", rec.Code)
	}

	asli, err := app.store.UserByUsername(ctx, "asli")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.DeleteUser(ctx, asli.ID); err != nil {
		t.Fatal(err)
	}
	mert := app.login("mert")
	checkBody(t, app.get(chat, mert).Body.String(), []string{"Started by deleted account"}, nil)
	checkBody(t, app.get(projectPath(app.project.ID)+"/chats", mert).Body.String(),
		[]string{"deleted agent", "deleted account", "Who am I?"}, nil)
}

func TestFailedAnswerIsSafe(t *testing.T) {
	app := newChatApp(t)
	app.host.mu.Lock()
	app.host.status = http.StatusUnauthorized
	app.host.replies = []string{`{"error": {"message": "Key ` + modelKey + ` is not valid"}}`}
	app.host.mu.Unlock()
	chat := app.startChat("asli", app.agent.ID, "Hello?")
	body := app.get(chat, app.login("asli")).Body.String()
	checkBody(t, body, []string{"The model&#39;s host failed: the host answered 401 Unauthorized: Key [hidden] is not valid."},
		[]string{"secretvalue"})
	if strings.Contains(app.logs.String(), "secretvalue") {
		t.Errorf("the log holds the key:\n%s", app.logs)
	}
}

func TestAnswerLinksStayText(t *testing.T) {
	app := newChatApp(t)
	app.host.script(answerJSON("Read [the guide](https://evil.example/steal?c=1) or <https://evil.example/auto>.\n\n" +
		"![logo](https://evil.example/pixel.png)\n\n<script>alert(1)</script><a href=\"https://evil.example/raw\">raw</a>"))
	chat := app.startChat("asli", app.agent.ID, "Links?")
	body := app.get(chat, app.login("asli")).Body.String()
	checkBody(t, body, []string{
		"the guide (https://evil.example/steal?c=1)",
		"https://evil.example/auto",
		"logo (image not shown: https://evil.example/pixel.png)",
	}, []string{`href="https://evil`, "<img", "<script>alert", "javascript:"})
}

func TestRenderAnswer(t *testing.T) {
	tests := []struct {
		md, want string
	}{
		{"**bold** and `code`", "<p><strong>bold</strong> and <code>code</code></p>\n"},
		{"[x](javascript:alert(1))", "<p>x (javascript:alert(1))</p>\n"},
		{"[x](https://a.example/?a=1&b=\"2\")", "<p>x (https://a.example/?a=1&amp;b=&quot;2&quot;)</p>\n"},
		{"<https://a.example>", "<p>https://a.example</p>\n"},
		{"Visit https://a.example now", "<p>Visit https://a.example now</p>\n"},
		{"![alt <b>](https://a.example/i.png)", "<p>alt <!-- raw HTML omitted --> (image not shown: https://a.example/i.png)</p>\n"},
		{"<img src=x onerror=alert(1)>", "<!-- raw HTML omitted -->\n"},
		{"a <span onclick=x>b</span>", "<p>a <!-- raw HTML omitted -->b<!-- raw HTML omitted --></p>\n"},
		{"| a | b |\n|---|---|\n| 1 | 2 |", "<table>\n<thead>\n<tr>\n<th>a</th>\n<th>b</th>\n</tr>\n</thead>\n<tbody>\n<tr>\n<td>1</td>\n<td>2</td>\n</tr>\n</tbody>\n</table>\n"},
	}
	for _, tt := range tests {
		if got := string(renderAnswer(tt.md)); got != tt.want {
			t.Errorf("renderAnswer(%q) =\n%q\nwant\n%q", tt.md, got, tt.want)
		}
	}
}
