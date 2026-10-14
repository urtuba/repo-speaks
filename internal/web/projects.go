package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// maxProjectName is the longest project name, in characters.
const maxProjectName = 100

// home lists the projects the account can open; the owner sees all of
// them. Visitors who are not logged in go to the login page, which sends
// a fresh instance on to setup.
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data := struct {
		User     *store.User
		Projects []store.Project
	}{User: &sess.user}

	var err error
	if sess.user.IsOwner {
		data.Projects, err = s.store.AllProjects(r.Context())
	} else {
		data.Projects, err = s.store.UserProjects(r.Context(), sess.user.ID)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "home.html", data)
}

// createProject creates a project and opens it.
func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	// For now only the instance owner creates projects. Who else may
	// create them is not decided yet.
	if !currentSession(r).user.IsOwner {
		http.Error(w, "Only the instance owner can create projects.", http.StatusForbidden)
		return
	}
	if !parseForm(w, r) {
		return
	}
	name, msg := projectName(r.PostFormValue("name"))
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	p, err := s.store.CreateProject(r.Context(), name, s.now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID), http.StatusSeeOther)
}

// projectPerms returns what the account may do in the project. It
// returns store.ErrNotFound when the account is not a member; the
// instance owner is always allowed in.
func (s *server) projectPerms(ctx context.Context, user store.User, projectID int64) (permissions.Set, error) {
	if user.IsOwner {
		return permissions.Effective(true, nil), nil
	}
	names, err := s.store.MemberPermissions(ctx, projectID, user.ID)
	if err != nil {
		return nil, err
	}
	return permissions.ParseSet(names)
}

// openProject loads the project in the URL and the viewer's permissions
// in it. A visitor who is not a member gets the same 404 as for a project
// that does not exist, so the answer does not reveal which projects
// exist. It returns false when it has already answered.
func (s *server) openProject(w http.ResponseWriter, r *http.Request) (store.Project, permissions.Set, bool) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Project{}, nil, false
	}
	perms, err := s.projectPerms(ctx, currentSession(r).user, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Project{}, nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.Project{}, nil, false
	}
	p, err := s.store.Project(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Project{}, nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.Project{}, nil, false
	}
	return p, perms, true
}

// project shows one project: the viewer's permissions, the agents, the
// resources (with resources:view), the members (with members:view) and
// the invite links (with members:edit).
func (s *server) project(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	notice := r.URL.Query().Get("notice")
	s.renderProjectPage(w, r, http.StatusOK, p, perms, projectPage{
		Notice:      resourceNotices[notice],
		AgentNotice: agentNotices[notice],
	})
}

// projectPage is the data for project.html.
type projectPage struct {
	User          *store.User
	Project       store.Project
	Permissions   []permissions.Permission
	IsMember      bool // false for the owner, who has no membership to leave
	CanEdit       bool // project:edit, for the settings link
	CanSeeMembers bool
	Members       []memberView
	CanInvite     bool
	Presets       []permissions.Preset // the presets the viewer may invite with
	Invites       []store.Invite
	Error         string // shown in the invite section

	// Every member sees the agents, without their prompts, and may ask
	// the ready ones. With agents:edit, the list links to the agent
	// pages.
	Agents        []store.AgentSummary
	CanEditAgents bool
	CanSeeChats   bool   // chats:view, for the link to every chat
	AgentNotice   string // after an agent action, such as "Agent deleted."

	CanSeeResources bool
	Resources       resourceList
	Notice          string // after a resource action, such as "Sync queued."
	// The add form, sent back after an error. The token is never sent back.
	ResourceError string
	RepoInput     string
	BranchInput   string
}

// renderProject renders the project page with an optional error for the
// invite section.
func (s *server) renderProject(w http.ResponseWriter, r *http.Request, status int, p store.Project, perms permissions.Set, errMsg string) {
	s.renderProjectPage(w, r, status, p, perms, projectPage{Error: errMsg})
}

// renderProjectPage renders the project page. data holds what the
// request adds, such as a form error; the rest is loaded here.
func (s *server) renderProjectPage(w http.ResponseWriter, r *http.Request, status int, p store.Project, perms permissions.Set, data projectPage) {
	ctx := r.Context()
	sess := currentSession(r)
	data.User = &sess.user
	data.Project = p
	data.Permissions = perms.List()
	data.IsMember = !sess.user.IsOwner
	data.CanEdit = perms.Has(permissions.PermProjectEdit)
	data.CanSeeMembers = perms.Has(permissions.PermMembersView)
	data.CanInvite = perms.Has(permissions.PermMembersEdit)
	data.CanSeeResources = perms.Has(permissions.PermResourcesView)
	data.CanEditAgents = perms.Has(permissions.PermAgentsEdit)
	data.CanSeeChats = perms.Has(permissions.PermChatsView)
	var err error
	if data.Agents, err = s.store.Agents(ctx, p.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.CanSeeResources {
		if data.Resources, err = s.loadResources(r, p.ID, perms.Has(permissions.PermResourcesEdit)); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if data.CanSeeMembers {
		if data.Members, err = s.memberViews(r, p.ID, sess.user.ID, perms); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if data.CanInvite {
		for _, preset := range permissions.Presets() {
			if permissions.CanInvite(perms, preset) {
				data.Presets = append(data.Presets, preset)
			}
		}
		if data.Invites, err = s.store.OpenInvites(ctx, p.ID, s.now()); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, status, "project.html", data)
}

// memberView is a member as the project page shows it.
type memberView struct {
	UserID      int64
	Username    string
	IsOwner     bool                     // the instance owner, listed in every project
	IsViewer    bool                     // the person looking at the page
	CanManage   bool                     // the viewer may change or remove this member
	Role        string                   // the matching preset, or "custom"
	Permissions []permissions.Permission // in display order
}

// memberViews loads a project's members for display, as seen by the
// viewer with their permissions. The instance owner comes first: they
// have every permission in every project without a membership, and
// members should see who that is.
func (s *server) memberViews(r *http.Request, projectID, viewerID int64, viewer permissions.Set) ([]memberView, error) {
	ctx := r.Context()
	owner, err := s.store.Owner(ctx)
	if err != nil {
		return nil, err
	}
	members, err := s.store.Members(ctx, projectID)
	if err != nil {
		return nil, err
	}
	views := []memberView{{UserID: owner.ID, Username: owner.Username, IsOwner: true, IsViewer: owner.ID == viewerID}}
	for _, m := range members {
		if m.UserID == owner.ID {
			continue // already listed as the owner
		}
		set, err := permissions.ParseSet(m.Permissions)
		if err != nil {
			return nil, err
		}
		views = append(views, memberView{
			UserID:      m.UserID,
			Username:    m.Username,
			IsViewer:    m.UserID == viewerID,
			CanManage:   m.UserID != viewerID && permissions.CanManage(viewer, set),
			Role:        roleName(set),
			Permissions: set.List(),
		})
	}
	return views, nil
}

// projectPath is the URL of a project's page.
func projectPath(id int64) string {
	return "/projects/" + strconv.FormatInt(id, 10)
}
