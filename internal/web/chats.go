package web

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/urtuba/repo-speaks/internal/agent"
	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// Every member may ask the project's agents. A chat is read by the
// member who started it, and by members with chats:view. Anyone else
// gets 404, as for a chat that does not exist. Only the member who
// started a chat asks in it.
//
// These pages show an agent as store.AgentSummary or by name only, so
// they cannot show the agent's instructions.

const (
	// maxQuestion is the longest question, in characters.
	maxQuestion = 4000

	// maxAllChats is how many chats the "All chats" page lists.
	maxAllChats = 200
)

// askPage is the data for ask.html: an agent's page for asking, with
// the viewer's own chats with it.
type askPage struct {
	User     *store.User
	Project  store.Project
	Agent    store.AgentSummary
	Chats    []store.Chat
	Question string // the question as typed, after an error
	Error    string
}

// openAskAgent loads the project and the agent in the URL. Every member
// may ask; others get 404. It returns false when it has already
// answered.
func (s *server) openAskAgent(w http.ResponseWriter, r *http.Request) (store.Project, store.AgentSummary, bool) {
	p, _, ok := s.openProject(w, r)
	if !ok {
		return store.Project{}, store.AgentSummary{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("agent"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Project{}, store.AgentSummary{}, false
	}
	a, err := s.store.AgentSummary(r.Context(), p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Project{}, store.AgentSummary{}, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.Project{}, store.AgentSummary{}, false
	}
	return p, a, true
}

// renderAsk loads the viewer's chats with the agent into page and
// renders the ask page.
func (s *server) renderAsk(w http.ResponseWriter, r *http.Request, status int, page askPage) {
	sess := currentSession(r)
	var err error
	if page.Chats, err = s.store.UserChats(r.Context(), page.Project.ID, sess.user.ID, page.Agent.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	page.User = &sess.user
	s.render(w, status, "ask.html", page)
}

// askAgentPage shows the form to ask an agent, and the viewer's chats
// with it.
func (s *server) askAgentPage(w http.ResponseWriter, r *http.Request) {
	p, a, ok := s.openAskAgent(w, r)
	if !ok {
		return
	}
	s.renderAsk(w, r, http.StatusOK, askPage{Project: p, Agent: a})
}

// startChat asks an agent a first question in a new chat, and opens the
// chat while the answer runs.
func (s *server) startChat(w http.ResponseWriter, r *http.Request) {
	p, a, ok := s.openAskAgent(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	page := askPage{Project: p, Agent: a, Question: r.PostFormValue("question")}
	question, msg := parseQuestion(page.Question)
	if msg != "" {
		page.Error = msg
		s.renderAsk(w, r, http.StatusBadRequest, page)
		return
	}
	chatID, err := s.agent.Ask(r.Context(), agent.Question{
		ProjectID: p.ID, AgentID: a.ID, UserID: currentSession(r).user.ID, Text: question,
	})
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r) // the agent was deleted a moment ago
		return
	}
	if status, msg := askError(err); msg != "" {
		page.Error = msg
		s.renderAsk(w, r, status, page)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, chatPath(p.ID, chatID), http.StatusSeeOther)
}

// parseQuestion checks a question as typed. msg explains a problem, or
// is empty.
func parseQuestion(raw string) (question, msg string) {
	// Browsers send line breaks as CRLF, but count them as one character.
	question = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if question == "" {
		return "", "Type a question."
	}
	if utf8.RuneCountInString(question) > maxQuestion {
		return "", "A question can have at most 4000 characters."
	}
	return question, ""
}

// askError explains why a question was refused, with the status to
// answer. It returns an empty msg for any other error.
func askError(err error) (status int, msg string) {
	if rl, ok := errors.AsType[*agent.RateLimitError](err); ok {
		return http.StatusTooManyRequests, rl.Error()
	}
	switch {
	case errors.Is(err, agent.ErrNotReady):
		return http.StatusConflict, "This agent is not ready: it has no model. Ask an admin to set one."
	case errors.Is(err, agent.ErrOneQuestion):
		return http.StatusConflict, "This agent answers one question per chat. Ask a new question from the agent's page."
	case errors.Is(err, store.ErrAnswerRunning):
		return http.StatusConflict, "The last answer is still running. Ask again when it is done."
	}
	return 0, ""
}

// chatPage is the data for chat.html.
type chatPage struct {
	User    *store.User
	Project store.Project
	Chat    store.Chat
	Body    chatBody
}

// chatBody is the data for the "chat-body" template: the messages and
// the question box, which htmx reloads while an answer runs.
type chatBody struct {
	ProjectID int64
	ChatID    int64
	AgentName string // empty for a deleted agent
	Messages  []messageView
	Running   bool // an answer runs, so the body reloads itself
	// CanAsk is true for the member who started a chat with a chat
	// agent that still exists. The box is disabled while Running.
	CanAsk bool
	// Closed explains why the chat takes no questions, for the member
	// who started it.
	Closed   string
	AskPath  string // the agent's page, for a new question
	Question string // the question as typed, after an error
	Error    string
}

// messageView is a message as the chat page shows it.
type messageView struct {
	store.Message
	HTML    template.HTML // a done answer, rendered
	Sources []sourceView
}

// sourceView is a source with its GitHub link.
type sourceView struct {
	store.Source
	URL string // empty when no link can be built
}

// openChat loads the project and the chat in the URL, and checks that
// the viewer may read it: they started it, or hold chats:view. Others
// get 404. It returns false when it has already answered.
func (s *server) openChat(w http.ResponseWriter, r *http.Request) (store.Project, store.Chat, bool) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return store.Project{}, store.Chat{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("chat"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Project{}, store.Chat{}, false
	}
	c, err := s.store.Chat(r.Context(), p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Project{}, store.Chat{}, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.Project{}, store.Chat{}, false
	}
	if !canReadChat(currentSession(r).user, perms, c) {
		http.NotFound(w, r)
		return store.Project{}, store.Chat{}, false
	}
	return p, c, true
}

// canReadChat reports whether the account may read the chat.
func canReadChat(user store.User, perms permissions.Set, c store.Chat) bool {
	return (c.UserID != 0 && c.UserID == user.ID) || perms.Has(permissions.PermChatsView)
}

// loadChatBody loads the chat's messages for display, as the viewer
// sees them.
func (s *server) loadChatBody(r *http.Request, c store.Chat) (chatBody, error) {
	ctx := r.Context()
	list, err := s.store.Messages(ctx, c.ProjectID, c.ID)
	if err != nil {
		return chatBody{}, err
	}
	body := chatBody{ProjectID: c.ProjectID, ChatID: c.ID, AgentName: c.AgentName}
	for _, m := range list {
		v := messageView{Message: m}
		if m.Status == store.AnswerRunning {
			body.Running = true
		}
		if m.Role == store.RoleAssistant && m.Status == store.AnswerDone {
			v.HTML = renderAnswer(m.Content)
			for _, src := range m.Sources {
				v.Sources = append(v.Sources, sourceView{Source: src, URL: agent.SourceURL(src)})
			}
		}
		body.Messages = append(body.Messages, v)
	}

	if c.UserID != currentSession(r).user.ID {
		return body, nil // a reader with chats:view asks nothing here
	}
	if c.AgentID == 0 {
		body.Closed = "The agent was deleted, so this chat takes no new questions."
		return body, nil
	}
	body.AskPath = askPath(c.ProjectID, c.AgentID)
	a, err := s.store.AgentSummary(ctx, c.ProjectID, c.AgentID)
	if errors.Is(err, store.ErrNotFound) {
		body.Closed = "The agent was deleted, so this chat takes no new questions."
		body.AskPath = ""
		return body, nil
	}
	if err != nil {
		return chatBody{}, err
	}
	switch {
	case a.Kind == store.AgentAnswer:
		body.Closed = "This agent answers one question per chat."
	case !a.Ready:
		body.Closed = "This agent is not ready: it has no model. Ask an admin to set one."
	default:
		body.CanAsk = true
	}
	return body, nil
}

// renderChat loads the chat's messages and renders the chat page, with
// the question as typed and an error, when the request had one.
func (s *server) renderChat(w http.ResponseWriter, r *http.Request, status int, p store.Project, c store.Chat, question, errMsg string) {
	body, err := s.loadChatBody(r, c)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	body.Question, body.Error = question, errMsg
	s.render(w, status, "chat.html", chatPage{User: &currentSession(r).user, Project: p, Chat: c, Body: body})
}

// chatPageView shows a chat.
func (s *server) chatPageView(w http.ResponseWriter, r *http.Request) {
	p, c, ok := s.openChat(w, r)
	if !ok {
		return
	}
	s.renderChat(w, r, http.StatusOK, p, c, "", "")
}

// chatBodyFragment renders the chat's messages alone, for htmx to swap
// in while an answer runs.
func (s *server) chatBodyFragment(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.openChat(w, r)
	if !ok {
		return
	}
	body, err := s.loadChatBody(r, c)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "chat-body", body)
}

// askInChat asks a follow-up question in a chat. Only the member who
// started the chat may; a reader with chats:view gets 403.
func (s *server) askInChat(w http.ResponseWriter, r *http.Request) {
	p, c, ok := s.openChat(w, r)
	if !ok {
		return
	}
	user := currentSession(r).user
	if c.UserID != user.ID {
		http.Error(w, "You can read this chat, but only the member who started it can ask in it.", http.StatusForbidden)
		return
	}
	if !parseForm(w, r) {
		return
	}
	typed := r.PostFormValue("question")
	question, msg := parseQuestion(typed)
	if msg != "" {
		s.renderChat(w, r, http.StatusBadRequest, p, c, typed, msg)
		return
	}
	if c.AgentID == 0 {
		s.renderChat(w, r, http.StatusConflict, p, c, typed, "The agent was deleted, so this chat takes no new questions.")
		return
	}
	_, err := s.agent.Ask(r.Context(), agent.Question{
		ProjectID: p.ID, AgentID: c.AgentID, UserID: user.ID, ChatID: c.ID, Text: question,
	})
	if errors.Is(err, store.ErrNotFound) {
		// The agent or the chat was deleted a moment ago.
		s.renderChat(w, r, http.StatusConflict, p, c, typed, "The agent was deleted, so this chat takes no new questions.")
		return
	}
	if status, msg := askError(err); msg != "" {
		s.renderChat(w, r, status, p, c, typed, msg)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, chatPath(p.ID, c.ID), http.StatusSeeOther)
}

// allChats lists every chat of the project, newest first. It needs
// chats:view.
func (s *server) allChats(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	if !perms.Has(permissions.PermChatsView) {
		http.Error(w, "You cannot read other members' chats in this project.", http.StatusForbidden)
		return
	}
	chats, err := s.store.ProjectChats(r.Context(), p.ID, maxAllChats+1)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := struct {
		User    *store.User
		Project store.Project
		Chats   []store.Chat
		More    bool // older chats exist that the page leaves out
		Max     int
	}{User: &currentSession(r).user, Project: p, Chats: chats, Max: maxAllChats}
	if len(chats) > maxAllChats {
		data.Chats, data.More = chats[:maxAllChats], true
	}
	s.render(w, http.StatusOK, "all_chats.html", data)
}

// askPath is the URL of an agent's page for asking.
func askPath(projectID, agentID int64) string {
	return agentPath(projectID, agentID) + "/chats"
}

// chatPath is the URL of a chat.
func chatPath(projectID, chatID int64) string {
	return projectPath(projectID) + "/chats/" + strconv.FormatInt(chatID, 10)
}
