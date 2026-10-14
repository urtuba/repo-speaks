package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// Every member sees a project's agents on the project page: name, kind,
// description and whether they are ready. That list holds
// store.AgentSummary, which has no prompt. The agent pages show the
// prompt, and they need agents:edit.

// agentNotices are the messages the project page shows after an agent
// action, by the notice name in the URL, like resourceNotices.
var agentNotices = map[string]string{
	"agent-deleted": "Agent deleted.",
}

// Limits on an agent's fields.
const (
	maxAgentName        = 100
	maxAgentDescription = 300
	maxAgentPrompt      = 20000
	// The rate limit is questions per member per hour. The agent
	// package counts them.
	defaultRateLimit = 20
	minRateLimit     = 1
	maxRateLimit     = 1000
)

// agentKind is one choice on the create page.
type agentKind struct {
	Value string
	Label string
}

// agentKinds are the kinds, in the order the create page lists them.
var agentKinds = []agentKind{
	{store.AgentChat, "Chat: a conversation, with follow-up questions"},
	{store.AgentAnswer, "Answer: one question, one answer in a set format"},
}

// agentForm is the create and edit form, as sent, so a form with an
// error shows what was typed.
type agentForm struct {
	Kind         string
	Name         string
	Description  string
	Prompt       string
	ModelID      int64
	RateLimit    string
	AllResources bool
	Picked       map[int64]bool // the ticked resources
}

// formFromAgent fills the form with the agent's values.
func formFromAgent(a store.Agent) agentForm {
	f := agentForm{
		Kind: a.Kind, Name: a.Name, Description: a.Description, Prompt: a.Prompt,
		ModelID: a.ModelID, RateLimit: strconv.Itoa(a.RateLimitPerHour),
		AllResources: a.AllResources, Picked: map[int64]bool{},
	}
	for _, id := range a.ResourceIDs {
		f.Picked[id] = true
	}
	return f
}

// readAgentForm reads the form as sent, for showing it again. It checks
// nothing: parseAgentForm does.
func readAgentForm(r *http.Request) agentForm {
	f := agentForm{
		Kind:         r.PostFormValue("kind"),
		Name:         r.PostFormValue("name"),
		Description:  r.PostFormValue("description"),
		Prompt:       r.PostFormValue("prompt"),
		RateLimit:    r.PostFormValue("rate_limit"),
		AllResources: r.PostFormValue("resources") != "picked",
		Picked:       map[int64]bool{},
	}
	f.ModelID, _ = strconv.ParseInt(r.PostFormValue("model"), 10, 64)
	for _, v := range r.PostForm["resource"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Picked[id] = true
		}
	}
	return f
}

// parseAgentForm reads and checks the form against the project's models
// and resources. msg explains a problem, or is empty. It leaves out the
// kind, which only the create form has.
func parseAgentForm(r *http.Request, models []store.Model, resources []store.Resource) (set store.AgentSettings, msg string) {
	set.Name = strings.TrimSpace(r.PostFormValue("name"))
	if n := utf8.RuneCountInString(set.Name); n == 0 || n > maxAgentName {
		return set, "An agent name must be 1 to 100 characters."
	}
	set.Description = strings.TrimSpace(r.PostFormValue("description"))
	if utf8.RuneCountInString(set.Description) > maxAgentDescription {
		return set, "A description can have at most 300 characters."
	}
	// Browsers send line breaks as CRLF, but count them as one character.
	set.Prompt = strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("prompt"), "\r\n", "\n"))
	if utf8.RuneCountInString(set.Prompt) > maxAgentPrompt {
		return set, "The instructions can have at most 20000 characters."
	}
	model, err := strconv.ParseInt(r.PostFormValue("model"), 10, 64)
	known := model == 0 || slices.ContainsFunc(models, func(m store.Model) bool { return m.ID == model })
	if err != nil || !known {
		return set, "Pick the project default or one of the project's models."
	}
	set.ModelID = model
	rate, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("rate_limit")))
	if err != nil || rate < minRateLimit || rate > maxRateLimit {
		return set, "The rate limit must be a whole number from 1 to 1000."
	}
	set.RateLimitPerHour = rate
	switch r.PostFormValue("resources") {
	case "all":
		set.AllResources = true
	case "picked":
		for _, v := range r.PostForm["resource"] {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || !slices.ContainsFunc(resources, func(res store.Resource) bool { return res.ID == id }) {
				return set, "Pick repositories of this project only."
			}
			set.ResourceIDs = append(set.ResourceIDs, id)
		}
		if len(set.ResourceIDs) == 0 {
			return set, "Tick at least one repository, or let the agent read all of them."
		}
	default:
		return set, "Choose whether the agent reads all repositories or only the ticked ones."
	}
	return set, ""
}

// agentPage is the data for agent.html, the create and edit page.
type agentPage struct {
	User    *store.User
	Project store.Project
	Agent   store.Agent // the saved agent; zero on the create page
	IsNew   bool
	Kinds   []agentKind
	// The choices: the project's models, the default model's name
	// (empty when the project has none), and its resources.
	Models       []store.Model
	DefaultModel string
	Resources    []store.Resource
	Form         agentForm
	Saved        string // "created" or "1"
	Error        string
}

// renderAgent loads the project's models and resources into page and
// renders the agent page.
func (s *server) renderAgent(w http.ResponseWriter, r *http.Request, status int, page agentPage) {
	var err error
	if page.Models, page.Resources, err = s.agentChoices(r, page.Project.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, m := range page.Models {
		if m.ID == page.Project.DefaultModelID {
			page.DefaultModel = m.Name
		}
	}
	page.User = &currentSession(r).user
	page.Kinds = agentKinds
	s.render(w, status, "agent.html", page)
}

// agentChoices loads the models and resources an agent may pick.
func (s *server) agentChoices(r *http.Request, projectID int64) ([]store.Model, []store.Resource, error) {
	models, err := s.store.Models(r.Context(), projectID)
	if err != nil {
		return nil, nil, err
	}
	resources, err := s.store.Resources(r.Context(), projectID)
	if err != nil {
		return nil, nil, err
	}
	return models, resources, nil
}

// openAgents loads the project in the URL and checks that the viewer
// has agents:edit. It returns false when it has already answered.
func (s *server) openAgents(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return store.Project{}, false
	}
	if !perms.Has(permissions.PermAgentsEdit) {
		http.Error(w, "You cannot change this project's agents.", http.StatusForbidden)
		return store.Project{}, false
	}
	return p, true
}

// openAgent loads the project and the agent in the URL, and checks that
// the viewer has agents:edit. An agent of another project gets 404,
// like a missing one. It returns false when it has already answered.
func (s *server) openAgent(w http.ResponseWriter, r *http.Request) (store.Project, store.Agent, bool) {
	p, ok := s.openAgents(w, r)
	if !ok {
		return store.Project{}, store.Agent{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("agent"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Project{}, store.Agent{}, false
	}
	a, err := s.store.Agent(r.Context(), p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Project{}, store.Agent{}, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.Project{}, store.Agent{}, false
	}
	return p, a, true
}

// newAgentPage shows the empty create form. It needs agents:edit.
func (s *server) newAgentPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.openAgents(w, r)
	if !ok {
		return
	}
	s.renderAgent(w, r, http.StatusOK, agentPage{
		Project: p, IsNew: true,
		Form: agentForm{
			Kind: store.AgentChat, RateLimit: strconv.Itoa(defaultRateLimit),
			AllResources: true, Picked: map[int64]bool{},
		},
	})
}

// addAgent creates an agent and opens its page. It needs agents:edit.
// An agent with no model can be saved; it is not ready until a model
// applies.
func (s *server) addAgent(w http.ResponseWriter, r *http.Request) {
	p, ok := s.openAgents(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	page := agentPage{Project: p, IsNew: true, Form: readAgentForm(r)}
	refuse := func(status int, msg string) {
		page.Error = msg
		s.renderAgent(w, r, status, page)
	}
	if page.Form.Kind != store.AgentChat && page.Form.Kind != store.AgentAnswer {
		refuse(http.StatusBadRequest, "Pick a kind: chat or answer.")
		return
	}
	models, resources, err := s.agentChoices(r, p.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	set, msg := parseAgentForm(r, models, resources)
	if msg != "" {
		refuse(http.StatusBadRequest, msg)
		return
	}
	a, err := s.store.AddAgent(r.Context(), p.ID, page.Form.Kind, set, s.now())
	switch {
	case errors.Is(err, store.ErrAgentExists):
		refuse(http.StatusConflict, "The project already has an agent with this name.")
	case errors.Is(err, store.ErrNotInProject):
		refuse(http.StatusBadRequest, "A model or repository you picked was just removed. Check the form and save again.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, agentPath(p.ID, a.ID)+"?saved=created", http.StatusSeeOther)
	}
}

// agentPageView shows an agent with a form to change it. It needs
// agents:edit.
func (s *server) agentPageView(w http.ResponseWriter, r *http.Request) {
	p, a, ok := s.openAgent(w, r)
	if !ok {
		return
	}
	page := agentPage{Project: p, Agent: a, Form: formFromAgent(a)}
	if saved := r.URL.Query().Get("saved"); saved == "1" || saved == "created" {
		page.Saved = saved
	}
	s.renderAgent(w, r, http.StatusOK, page)
}

// saveAgent changes an agent. The kind stays as it was created.
func (s *server) saveAgent(w http.ResponseWriter, r *http.Request) {
	p, a, ok := s.openAgent(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	page := agentPage{Project: p, Agent: a, Form: readAgentForm(r)}
	page.Form.Kind = a.Kind
	refuse := func(status int, msg string) {
		page.Error = msg
		s.renderAgent(w, r, status, page)
	}
	models, resources, err := s.agentChoices(r, p.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	set, msg := parseAgentForm(r, models, resources)
	if msg != "" {
		refuse(http.StatusBadRequest, msg)
		return
	}
	err = s.store.UpdateAgent(r.Context(), p.ID, a.ID, set, s.now())
	switch {
	case errors.Is(err, store.ErrAgentExists):
		refuse(http.StatusConflict, "The project already has an agent with this name.")
	case errors.Is(err, store.ErrNotInProject):
		refuse(http.StatusBadRequest, "A model or repository you picked was just removed. Check the form and save again.")
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r) // deleted while the form was checked
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, agentPath(p.ID, a.ID)+"?saved=1", http.StatusSeeOther)
	}
}

// deleteAgent removes an agent from the project. It needs agents:edit.
func (s *server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	p, a, ok := s.openAgent(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteAgent(r.Context(), p.ID, a.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"?notice=agent-deleted#agents", http.StatusSeeOther)
}

// agentPath is the URL of an agent's page.
func agentPath(projectID, agentID int64) string {
	return projectPath(projectID) + "/agents/" + strconv.FormatInt(agentID, 10)
}

// nameList joins names for a sentence: "A", "A and B", "A, B and C".
func nameList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
