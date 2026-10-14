package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testCommit = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"

// chatFixture is the resource fixture with an account, a repository and
// a chat agent in the first project, and an agent in the second.
type chatFixture struct {
	resourceFixture
	user       User
	res        Resource
	agent      Agent
	otherAgent Agent
	otherRes   Resource
}

func newChatFixture(t *testing.T) chatFixture {
	t.Helper()
	ctx := context.Background()
	f := chatFixture{resourceFixture: newResourceFixture(t)}
	var err error
	if f.user, err = f.s.CreateUser(ctx, "asli", "hash", testNow); err != nil {
		t.Fatal(err)
	}
	f.res = f.add(t, "urtuba", "demo", "", "")
	f.agent = f.addAgent(t, AgentChat, "Helper")
	f.otherAgent, err = f.s.AddAgent(ctx, f.other.ID, AgentChat, AgentSettings{
		Name: "Billing helper", RateLimitPerHour: 20, AllResources: true,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	f.otherRes, err = f.s.AddResource(ctx, NewResource{ProjectID: f.other.ID, Owner: "urtuba", Repo: "billing"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// start starts a chat as the fixture's account with its agent.
func (f chatFixture) start(t *testing.T, question string) (chatID, answerID int64) {
	t.Helper()
	chatID, answerID, err := f.s.StartChat(context.Background(), f.project.ID, f.agent.ID, f.user.ID, question, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return chatID, answerID
}

// messages returns the chat's messages.
func (f chatFixture) messages(t *testing.T, chatID int64) []Message {
	t.Helper()
	list, err := f.s.Messages(context.Background(), f.project.ID, chatID)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestStartChat(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	chatID, answerID := f.start(t, "Why can't clients\ncancel after 24 hours?")

	c, err := f.s.Chat(ctx, f.project.ID, chatID)
	if err != nil {
		t.Fatal(err)
	}
	want := Chat{
		ID: chatID, ProjectID: f.project.ID, AgentID: f.agent.ID, AgentName: "Helper", AgentKind: AgentChat,
		UserID: f.user.ID, Username: "asli", Title: "Why can't clients cancel after 24 hours?", CreatedAt: testNow,
	}
	if c != want {
		t.Errorf("Chat() = %+v, want %+v", c, want)
	}

	list := f.messages(t, chatID)
	if len(list) != 2 {
		t.Fatalf("got %d messages, want 2", len(list))
	}
	q, a := list[0], list[1]
	if q.Role != RoleUser || q.Content != "Why can't clients\ncancel after 24 hours?" || q.Status != "" {
		t.Errorf("question = %+v", q)
	}
	if a.ID != answerID || a.Role != RoleAssistant || a.Status != AnswerRunning || a.Content != "" || !a.FinishedAt.IsZero() {
		t.Errorf("answer = %+v", a)
	}

	// An agent of another project, or a missing one, starts no chat.
	for _, id := range []int64{f.otherAgent.ID, 999} {
		_, _, err := f.s.StartChat(ctx, f.project.ID, id, f.user.ID, "Hello?", testNow)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("StartChat(agent %d) error = %v, want ErrNotFound", id, err)
		}
	}
	// One project's ID cannot reach another project's chat.
	if _, err := f.s.Chat(ctx, f.other.ID, chatID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Chat(other project) error = %v, want ErrNotFound", err)
	}
	if list, err := f.s.Messages(ctx, f.other.ID, chatID); err != nil || len(list) != 0 {
		t.Errorf("Messages(other project) = %v, %v, want none", list, err)
	}
}

func TestChatTitle(t *testing.T) {
	long := strings.Repeat("word ", 30)
	tests := []struct{ question, want string }{
		{"  How do refunds work?  ", "How do refunds work?"},
		{"Line one\n\nline two\ttab", "Line one line two tab"},
		{long, strings.TrimSpace(long[:maxTitle]) + "..."},
		{strings.Repeat("ç", 100), strings.Repeat("ç", maxTitle) + "..."},
	}
	for _, tt := range tests {
		if got := chatTitle(tt.question); got != tt.want {
			t.Errorf("chatTitle(%q) = %q, want %q", tt.question, got, tt.want)
		}
	}
}

func TestAddQuestion(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	chatID, answerID := f.start(t, "First?")

	// The first answer still runs.
	if _, err := f.s.AddQuestion(ctx, f.project.ID, chatID, "Second?", testNow); !errors.Is(err, ErrAnswerRunning) {
		t.Fatalf("AddQuestion while running: error = %v, want ErrAnswerRunning", err)
	}
	if err := f.s.FinishAnswer(ctx, f.project.ID, answerID, "Yes.", nil, AnswerUsage{}, testNow); err != nil {
		t.Fatal(err)
	}
	second, err := f.s.AddQuestion(ctx, f.project.ID, chatID, "Second?", testNow)
	if err != nil {
		t.Fatal(err)
	}
	list := f.messages(t, chatID)
	if len(list) != 4 || list[2].Content != "Second?" || list[3].ID != second || list[3].Status != AnswerRunning {
		t.Errorf("messages = %+v", list)
	}

	// Another project's ID, or a missing chat.
	if _, err := f.s.AddQuestion(ctx, f.other.ID, chatID, "Hi?", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddQuestion(other project) error = %v, want ErrNotFound", err)
	}
	if _, err := f.s.AddQuestion(ctx, f.project.ID, 999, "Hi?", testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddQuestion(missing chat) error = %v, want ErrNotFound", err)
	}
}

func TestFinishAnswer(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	chatID, answerID := f.start(t, "How do refunds work?")
	later := testNow.Add(time.Minute)

	for _, step := range []string{"Searching for refund", "Reading urtuba/demo/app/refund.go\nsecond line"} {
		if err := f.s.AddAnswerStep(ctx, f.project.ID, answerID, step); err != nil {
			t.Fatal(err)
		}
	}
	// Another project's ID cannot touch the answer.
	if err := f.s.AddAnswerStep(ctx, f.other.ID, answerID, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddAnswerStep(other project) error = %v, want ErrNotFound", err)
	}
	if err := f.s.FinishAnswer(ctx, f.other.ID, answerID, "x", nil, AnswerUsage{}, later); !errors.Is(err, ErrNotFound) {
		t.Errorf("FinishAnswer(other project) error = %v, want ErrNotFound", err)
	}

	usage := AnswerUsage{ModelID: testModelID, PromptTokens: 1200, CompletionTokens: 80, Steps: 2}
	sources := []NewSource{
		{ResourceID: f.res.ID, Path: "app/refund.go", LineFrom: 10, LineTo: 24, CommitID: testCommit},
		{ResourceID: f.otherRes.ID, Path: "secret.go", LineFrom: 1, LineTo: 2, CommitID: testCommit}, // another project's
		{ResourceID: f.res.ID, Path: "README.md", LineFrom: 3, LineTo: 3, CommitID: testCommit},
	}
	if err := f.s.FinishAnswer(ctx, f.project.ID, answerID, "Refunds take 5 days.", sources, usage, later); err != nil {
		t.Fatal(err)
	}
	a := f.messages(t, chatID)[1]
	if a.Status != AnswerDone || a.Content != "Refunds take 5 days." || a.Usage != usage || !a.FinishedAt.Equal(later) {
		t.Errorf("answer = %+v", a)
	}
	wantProgress := []string{"Searching for refund", "Reading urtuba/demo/app/refund.go second line"}
	if !reflect.DeepEqual(a.Progress, wantProgress) {
		t.Errorf("Progress = %q, want %q", a.Progress, wantProgress)
	}
	wantSources := []Source{
		{Position: 1, ResourceID: f.res.ID, Owner: "urtuba", Repo: "demo", Path: "app/refund.go", LineFrom: 10, LineTo: 24, CommitID: testCommit},
		{Position: 2, ResourceID: f.res.ID, Owner: "urtuba", Repo: "demo", Path: "README.md", LineFrom: 3, LineTo: 3, CommitID: testCommit},
	}
	if !reflect.DeepEqual(a.Sources, wantSources) {
		t.Errorf("Sources = %+v\nwant %+v", a.Sources, wantSources)
	}

	// A finished answer does not change again.
	if err := f.s.FailAnswer(ctx, f.project.ID, answerID, "late", AnswerUsage{}, later); !errors.Is(err, ErrNotFound) {
		t.Errorf("FailAnswer after done: error = %v, want ErrNotFound", err)
	}
	if err := f.s.AddAnswerStep(ctx, f.project.ID, answerID, "late"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddAnswerStep after done: error = %v, want ErrNotFound", err)
	}

	// A deleted resource takes its sources along; the text stays.
	if err := f.s.DeleteResource(ctx, f.project.ID, f.res.ID); err != nil {
		t.Fatal(err)
	}
	a = f.messages(t, chatID)[1]
	if len(a.Sources) != 0 || a.Content != "Refunds take 5 days." {
		t.Errorf("after the resource is deleted: answer = %+v", a)
	}
}

func TestFailAnswer(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	chatID, answerID := f.start(t, "How do refunds work?")
	usage := AnswerUsage{ModelID: testModelID, PromptTokens: 10, Steps: 1}
	if err := f.s.FailAnswer(ctx, f.project.ID, answerID, "the host refused the connection", usage, testNow); err != nil {
		t.Fatal(err)
	}
	a := f.messages(t, chatID)[1]
	if a.Status != AnswerFailed || a.Error != "the host refused the connection" || a.Usage != usage || a.FinishedAt.IsZero() {
		t.Errorf("answer = %+v", a)
	}
	// The chat takes a new question after a failed answer.
	if _, err := f.s.AddQuestion(ctx, f.project.ID, chatID, "Again?", testNow); err != nil {
		t.Errorf("AddQuestion after a failed answer: %v", err)
	}
}

func TestFailRunningAnswers(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	chat1, running1 := f.start(t, "One?")
	f.start(t, "Two?")
	chat3, done := f.start(t, "Three?")
	if err := f.s.FinishAnswer(ctx, f.project.ID, done, "Done.", nil, AnswerUsage{}, testNow); err != nil {
		t.Fatal(err)
	}
	n, err := f.s.FailRunningAnswers(ctx, "the server restarted", testNow)
	if err != nil || n != 2 {
		t.Fatalf("FailRunningAnswers() = %d, %v, want 2", n, err)
	}
	if a := f.messages(t, chat1)[1]; a.ID != running1 || a.Status != AnswerFailed || a.Error != "the server restarted" {
		t.Errorf("running answer = %+v", a)
	}
	if a := f.messages(t, chat3)[1]; a.Status != AnswerDone {
		t.Errorf("done answer = %+v, want it unchanged", a)
	}
}

func TestChatLists(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	mert, err := f.s.CreateUser(ctx, "mert", "hash", testNow)
	if err != nil {
		t.Fatal(err)
	}
	second := f.addAgent(t, AgentAnswer, "Triage")
	c1, _ := f.start(t, "One?")
	c2, _ := f.start(t, "Two?")
	c3, _, err := f.s.StartChat(ctx, f.project.ID, f.agent.ID, mert.ID, "Mert's?", testNow)
	if err != nil {
		t.Fatal(err)
	}
	c4, _, err := f.s.StartChat(ctx, f.project.ID, second.ID, f.user.ID, "Triage?", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.StartChat(ctx, f.other.ID, f.otherAgent.ID, f.user.ID, "Billing?", testNow); err != nil {
		t.Fatal(err)
	}

	ids := func(list []Chat, err error) []int64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, c := range list {
			out = append(out, c.ID)
		}
		return out
	}
	if got := ids(f.s.UserChats(ctx, f.project.ID, f.user.ID, f.agent.ID)); !reflect.DeepEqual(got, []int64{c2, c1}) {
		t.Errorf("UserChats() = %v, want %v", got, []int64{c2, c1})
	}
	if got := ids(f.s.ProjectChats(ctx, f.project.ID, 10)); !reflect.DeepEqual(got, []int64{c4, c3, c2, c1}) {
		t.Errorf("ProjectChats() = %v, want %v", got, []int64{c4, c3, c2, c1})
	}
	if got := ids(f.s.ProjectChats(ctx, f.project.ID, 2)); !reflect.DeepEqual(got, []int64{c4, c3}) {
		t.Errorf("ProjectChats(limit 2) = %v, want %v", got, []int64{c4, c3})
	}

	// Chats stay when the agent or the account is deleted.
	if err := f.s.DeleteAgent(ctx, f.project.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteUser(ctx, mert.ID); err != nil {
		t.Fatal(err)
	}
	c, err := f.s.Chat(ctx, f.project.ID, c3)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentID != 0 || c.AgentName != "" || c.UserID != 0 || c.Username != "" || c.Title != "Mert's?" {
		t.Errorf("chat after deletes = %+v", c)
	}
	if got := ids(f.s.ProjectChats(ctx, f.project.ID, 10)); len(got) != 4 {
		t.Errorf("ProjectChats() after deletes = %v, want 4 chats", got)
	}
	if len(f.messages(t, c3)) != 2 {
		t.Error("the messages of a chat with a deleted agent are gone")
	}

	// Deleting the project deletes its chats.
	if err := f.s.DeleteProject(ctx, f.project.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&n); err != nil || n != 2 {
		t.Errorf("messages left = %d, %v, want the other project's 2", n, err)
	}
}

func TestAgentSummary(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	a, err := f.s.AgentSummary(ctx, f.project.ID, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a != f.agent.AgentSummary {
		t.Errorf("AgentSummary() = %+v, want %+v", a, f.agent.AgentSummary)
	}
	if _, err := f.s.AgentSummary(ctx, f.project.ID, f.otherAgent.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("AgentSummary(other project) error = %v, want ErrNotFound", err)
	}
}
