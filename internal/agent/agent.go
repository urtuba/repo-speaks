// Package agent answers questions about a project's repositories. An
// answer is a loop: the model calls read-only tools over the clones of
// the agent's repositories until it can answer, then cites the files it
// used.
//
// The loop runs in the background, on the server's context, never on a
// request's. Each step is saved on the answer, so a page can show it and
// a reload loses nothing.
//
// Repository files can carry instructions for the model (prompt
// injection). So the tools only read, only inside the agent's own
// repositories, and every file access goes through os.Root, which no
// path or symlink can leave.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
	reposync "github.com/urtuba/repo-speaks/internal/sync"
)

const (
	// maxSteps is how many tool calls one answer may make. At the limit,
	// the model must answer with what it found.
	maxSteps = 30

	// timeLimit is how long one answer may run, from its start.
	timeLimit = 5 * time.Minute

	// parallel is how many answers run at the same time. More wait.
	parallel = 4

	// maxAnswerTokens caps each model reply.
	maxAnswerTokens = 8192

	// writeTimeout limits the database write that records how an answer
	// ended, which still runs while the server stops.
	writeTimeout = 5 * time.Second
)

// The reasons an answer fails, as the asker reads them.
const (
	msgRestarted = "The server restarted before the answer was done. Ask again."
	msgStopped   = "The server stopped before the answer was done. Ask again."
	msgInternal  = "Something went wrong on the server. Ask again, or tell an admin."
)

// ErrNotReady is returned for an agent with no model: neither its own
// nor the project's default.
var ErrNotReady = errors.New("the agent has no model, so it cannot answer")

// ErrOneQuestion is returned for a second question to an answer agent.
var ErrOneQuestion = errors.New("an answer agent takes one question per chat")

// answerError is a failure the asker should read as it is, such as a
// deleted agent.
type answerError struct{ msg string }

func (e *answerError) Error() string { return e.msg }

// Service starts answers and runs them. It is safe for use by many
// goroutines.
type Service struct {
	store   *store.Store
	llm     *llm.Client
	dataDir string
	logger  *slog.Logger
	now     func() time.Time
	limits  *limiter
	slots   chan struct{} // one value per running answer
	wg      sync.WaitGroup

	// base is the server's context, set by Start. Answers run on it.
	base context.Context

	// The limits, as fields so tests can lower them.
	maxSteps  int
	timeLimit time.Duration
}

// New returns a Service that reads clones under dataDir and calls
// models with client. Start must run before Ask.
func New(st *store.Store, client *llm.Client, dataDir string, logger *slog.Logger) *Service {
	return newService(st, client, dataDir, logger, time.Now)
}

// newService is New with a clock, for tests.
func newService(st *store.Store, client *llm.Client, dataDir string, logger *slog.Logger, now func() time.Time) *Service {
	return &Service{
		store:     st,
		llm:       client,
		dataDir:   dataDir,
		logger:    logger,
		now:       now,
		limits:    newLimiter(),
		slots:     make(chan struct{}, parallel),
		maxSteps:  maxSteps,
		timeLimit: timeLimit,
	}
}

// Start marks the answers an earlier run left running as failed, and
// runs answers on ctx from now on. Answers run inside this process, so
// one still running at start was cut off by a stop or a crash.
func (s *Service) Start(ctx context.Context) error {
	n, err := s.store.FailRunningAnswers(ctx, msgRestarted, s.now())
	if err != nil {
		return err
	}
	if n > 0 {
		s.logger.Info("marked answers cut off by a restart as failed", "count", n)
	}
	s.base = ctx
	return nil
}

// Wait blocks until every answer has ended. After the server's context
// ends, that takes a moment: each answer stops and records why.
func (s *Service) Wait() {
	s.wg.Wait()
}

// Question is a question a member asks an agent.
type Question struct {
	ProjectID int64
	AgentID   int64
	UserID    int64
	// ChatID is the chat to ask in, or 0 to start a new chat. Only the
	// member who started a chat asks in it.
	ChatID int64
	Text   string // checked by the caller
}

// Ask saves the question with a running answer and starts the answer in
// the background. It returns the chat's ID.
//
// It returns store.ErrNotFound when the project has no such agent, or
// no such chat of this member with this agent; ErrNotReady for an agent
// with no model; ErrOneQuestion for a second question to an answer
// agent; store.ErrAnswerRunning while the chat's last answer runs; and
// a *RateLimitError when the member asked too often.
func (s *Service) Ask(ctx context.Context, q Question) (int64, error) {
	if s.base == nil {
		return 0, errors.New("ask: the agent service has not started")
	}
	a, err := s.store.Agent(ctx, q.ProjectID, q.AgentID)
	if err != nil {
		return 0, err
	}
	if !a.Ready {
		return 0, ErrNotReady
	}
	if q.ChatID != 0 {
		c, err := s.store.Chat(ctx, q.ProjectID, q.ChatID)
		if err != nil {
			return 0, err
		}
		if c.UserID != q.UserID || c.AgentID != a.ID {
			return 0, fmt.Errorf("chat %d: %w", q.ChatID, store.ErrNotFound)
		}
		if a.Kind == store.AgentAnswer {
			return 0, ErrOneQuestion
		}
	}

	now := s.now()
	key := limitKey{agentID: a.ID, userID: q.UserID}
	if retry, ok := s.limits.take(key, a.RateLimitPerHour, now); !ok {
		return 0, &RateLimitError{Limit: a.RateLimitPerHour, RetryAt: retry}
	}
	chatID, answerID := q.ChatID, int64(0)
	if chatID == 0 {
		chatID, answerID, err = s.store.StartChat(ctx, q.ProjectID, a.ID, q.UserID, q.Text, now)
	} else {
		answerID, err = s.store.AddQuestion(ctx, q.ProjectID, chatID, q.Text, now)
	}
	if err != nil {
		s.limits.giveBack(key, now) // nothing was asked
		return 0, err
	}

	s.wg.Add(1)
	go s.run(job{projectID: q.ProjectID, agentID: a.ID, chatID: chatID, answerID: answerID})
	return chatID, nil
}

// job is one answer to run.
type job struct {
	projectID int64
	agentID   int64
	chatID    int64
	answerID  int64
}

// run waits for a free slot, runs the answer and records how it ended.
func (s *Service) run(j job) {
	defer s.wg.Done()
	var usage store.AnswerUsage
	defer func() {
		if v := recover(); v != nil {
			s.logger.Error("answer panicked", "answer", j.answerID, "err", v, "stack", string(debug.Stack()))
			s.fail(j, msgInternal, usage)
		}
	}()

	select {
	case s.slots <- struct{}{}:
	case <-s.base.Done():
		s.fail(j, msgStopped, usage)
		return
	}
	defer func() { <-s.slots }()

	ctx, cancel := context.WithTimeout(s.base, s.timeLimit)
	defer cancel()
	text, sources, err := s.answer(ctx, j, &usage)
	if err != nil {
		s.fail(j, s.failMessage(ctx, j, err), usage)
		return
	}
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(s.base), writeTimeout)
	defer wcancel()
	if err := s.store.FinishAnswer(wctx, j.projectID, j.answerID, text, sources, usage, s.now()); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		s.logger.Error("save answer", "answer", j.answerID, "err", err)
	}
}

// failMessage explains err to the asker. Errors from the model's host
// are safe to show; others are logged and shown in general words.
func (s *Service) failMessage(ctx context.Context, j job, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("The answer took longer than %s, so it was stopped. Try a narrower question.",
			minutes(s.timeLimit))
	case ctx.Err() != nil:
		return msgStopped
	}
	if ae, ok := errors.AsType[*answerError](err); ok {
		return ae.msg
	}
	if le, ok := errors.AsType[*llm.Error](err); ok {
		return "The model's host failed: " + le.Error() + "."
	}
	s.logger.Error("answer failed", "answer", j.answerID, "err", err)
	return msgInternal
}

// minutes writes a duration for the asker, such as "5 minutes".
func minutes(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d/time.Second))
	}
	if d == time.Minute {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// fail records a failed answer. It runs while the server stops too, so
// it does not use the server's context as it is.
func (s *Service) fail(j job, msg string, usage store.AnswerUsage) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.base), writeTimeout)
	defer cancel()
	err := s.store.FailAnswer(ctx, j.projectID, j.answerID, msg, usage, s.now())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("save failed answer", "answer", j.answerID, "err", err)
	}
}

// answer runs the loop. It adds what the calls used to usage, also when
// it fails.
func (s *Service) answer(ctx context.Context, j job, usage *store.AnswerUsage) (string, []store.NewSource, error) {
	a, err := s.store.Agent(ctx, j.projectID, j.agentID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, &answerError{"The agent was deleted before it could answer."}
	}
	if err != nil {
		return "", nil, err
	}
	model, err := s.model(ctx, j.projectID, a)
	if err != nil {
		return "", nil, err
	}
	usage.ModelID = model.ModelID

	tools, skipped, err := s.openRepos(ctx, j.projectID, a)
	if err != nil {
		return "", nil, err
	}
	defer tools.close()

	history, question, err := s.history(ctx, j)
	if err != nil {
		return "", nil, err
	}
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt(a.Prompt)},
		{Role: llm.RoleUser, Content: tools.overview(skipped)},
	}
	if a.Kind == store.AgentChat {
		messages = append(messages, history...)
	}
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: question})

	for {
		final := usage.Steps >= s.maxSteps
		if final {
			messages = append(messages, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(stepLimitNote, s.maxSteps)})
			if err := s.step(ctx, j, "Writing the answer from what was found"); err != nil {
				return "", nil, err
			}
		}
		reply, err := s.llm.Chat(ctx, model.BaseURL, model.APIKey, llm.ChatRequest{
			ModelID: model.ModelID, Messages: messages, Tools: toolDefs, NoTools: final,
			MaxTokens: maxAnswerTokens,
		})
		if err != nil {
			return "", nil, err
		}
		usage.PromptTokens += reply.PromptTokens
		usage.CompletionTokens += reply.CompletionTokens

		if final || len(reply.Message.ToolCalls) == 0 {
			text := strings.TrimSpace(reply.Message.Content)
			if text == "" {
				if reply.FinishReason == "length" {
					return "", nil, &answerError{"The model ran out of tokens before it answered."}
				}
				return "", nil, &answerError{"The model gave an empty answer."}
			}
			return text, tools.sources(text), nil
		}

		messages = append(messages, reply.Message)
		for _, call := range reply.Message.ToolCalls {
			// Every call needs a result, also those past the limit.
			result := fmt.Sprintf("Not run: you have used all %d tool steps.", s.maxSteps)
			if usage.Steps < s.maxSteps {
				usage.Steps++
				var step string
				step, result, err = tools.run(ctx, call)
				if err != nil {
					return "", nil, err
				}
				if err := s.step(ctx, j, step); err != nil {
					return "", nil, err
				}
			}
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: result})
		}
	}
}

// step records what the agent does now, for the asker's page.
func (s *Service) step(ctx context.Context, j job, text string) error {
	err := s.store.AddAnswerStep(ctx, j.projectID, j.answerID, text)
	if errors.Is(err, store.ErrNotFound) {
		// The chat or its project was deleted: stop spending tokens.
		return &answerError{"The chat was deleted."}
	}
	return err
}

// model returns the agent's own model, or the project's default.
func (s *Service) model(ctx context.Context, projectID int64, a store.Agent) (store.ModelEndpoint, error) {
	id := a.ModelID
	if id == 0 {
		p, err := s.store.Project(ctx, projectID)
		if err != nil {
			return store.ModelEndpoint{}, err
		}
		id = p.DefaultModelID
	}
	if id == 0 {
		return store.ModelEndpoint{}, &answerError{"The agent has no model. Ask an admin to set one."}
	}
	m, err := s.store.ModelEndpoint(ctx, projectID, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.ModelEndpoint{}, &answerError{"The agent's model was deleted. Ask an admin to set one."}
	}
	return m, err
}

// openRepos opens the clone of each repository the agent reads, as it
// is at question start. Only resources of the project come back from
// the store, and only the agent's picks among them are opened. It also
// returns the repositories never synced.
func (s *Service) openRepos(ctx context.Context, projectID int64, a store.Agent) (*toolbox, []string, error) {
	list, err := s.store.Resources(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	picked := map[int64]bool{}
	for _, id := range a.ResourceIDs {
		picked[id] = true
	}
	var mine []store.Resource
	repoCount := map[string]int{} // how often each owner/repo appears, in lower case
	for _, r := range list {
		if r.ProjectID != projectID || (!a.AllResources && !picked[r.ID]) {
			continue
		}
		mine = append(mine, r)
		repoCount[strings.ToLower(r.Owner+"/"+r.Repo)]++
	}

	t := &toolbox{read: map[fileKey]int{}}
	var skipped []string
	for _, r := range mine {
		label := r.Owner + "/" + r.Repo
		if repoCount[strings.ToLower(label)] > 1 && r.Branch != "" {
			label += "@" + r.Branch
		}
		// The folder name comes from the database. A sync writes only a
		// plain name, so anything else is refused.
		if r.CloneDir == "" || r.LastCommit == "" || filepath.Base(r.CloneDir) != r.CloneDir || r.CloneDir == ".." {
			skipped = append(skipped, label)
			continue
		}
		root, err := os.OpenRoot(reposync.ClonePath(s.dataDir, projectID, r.ID, r.CloneDir))
		if err != nil {
			s.logger.Error("open clone", "resource", r.ID, "err", err)
			skipped = append(skipped, label)
			continue
		}
		t.repos = append(t.repos, &repo{label: label, resourceID: r.ID, commit: r.LastCommit, root: root})
	}
	return t, skipped, nil
}

// history returns the earlier questions and done answers of the chat as
// text, and the question the answer is for. Tool results of earlier
// answers are not kept, so they are not sent again.
func (s *Service) history(ctx context.Context, j job) ([]llm.Message, string, error) {
	list, err := s.store.Messages(ctx, j.projectID, j.chatID)
	if err != nil {
		return nil, "", err
	}
	var out []llm.Message
	question := ""
	for _, m := range list {
		if m.ID == j.answerID {
			break
		}
		if m.Role == store.RoleUser {
			question = m.Content
			continue
		}
		if m.Status == store.AnswerDone && question != "" {
			out = append(out,
				llm.Message{Role: llm.RoleUser, Content: question},
				llm.Message{Role: llm.RoleAssistant, Content: m.Content})
		}
		question = ""
	}
	if question == "" {
		return nil, "", errors.New("the answer has no question before it")
	}
	return out, question, nil
}
