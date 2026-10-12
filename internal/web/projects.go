package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

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
	name := strings.TrimSpace(r.PostFormValue("name"))
	if n := utf8.RuneCountInString(name); n == 0 || n > maxProjectName {
		http.Error(w, "A project name must be 1 to 100 characters.", http.StatusBadRequest)
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

// project shows one project: the viewer's permissions, the members (with
// members:view) and the invite links (with members:edit).
func (s *server) project(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	s.renderProject(w, r, http.StatusOK, p, perms, "")
}

// projectPage is the data for project.html.
type projectPage struct {
	User          *store.User
	Project       store.Project
	Permissions   []permissions.Permission
	CanSeeMembers bool
	Members       []memberView
	CanInvite     bool
	Presets       []permissions.Preset // the presets the viewer may invite with
	Invites       []store.Invite
	Error         string // shown in the invite section
}

// renderProject renders the project page with an optional error for the
// invite section.
func (s *server) renderProject(w http.ResponseWriter, r *http.Request, status int, p store.Project, perms permissions.Set, errMsg string) {
	ctx := r.Context()
	sess := currentSession(r)
	data := projectPage{
		User:          &sess.user,
		Project:       p,
		Permissions:   perms.List(),
		CanSeeMembers: perms.Has(permissions.PermMembersView),
		CanInvite:     perms.Has(permissions.PermMembersEdit),
		Error:         errMsg,
	}
	var err error
	if data.CanSeeMembers {
		if data.Members, err = s.memberViews(r, p.ID); err != nil {
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
	Username    string
	Permissions []permissions.Permission // in display order
}

// memberViews loads a project's members for display.
func (s *server) memberViews(r *http.Request, projectID int64) ([]memberView, error) {
	members, err := s.store.Members(r.Context(), projectID)
	if err != nil {
		return nil, err
	}
	views := make([]memberView, len(members))
	for i, m := range members {
		set, err := permissions.ParseSet(m.Permissions)
		if err != nil {
			return nil, err
		}
		views[i] = memberView{Username: m.Username, Permissions: set.List()}
	}
	return views, nil
}

// projectPath is the URL of a project's page.
func projectPath(id int64) string {
	return "/projects/" + strconv.FormatInt(id, 10)
}
