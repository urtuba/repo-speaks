package web

import (
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

// home renders the start page. A logged-in account sees the projects it
// can open; the owner sees all of them.
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	data := struct {
		User       *store.User
		Projects   []store.Project
		NeedsSetup bool
	}{}

	sess := currentSession(r)
	if sess == nil {
		has, err := s.store.HasUsers(r.Context())
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		data.NeedsSetup = !has
		s.render(w, http.StatusOK, "home.html", data)
		return
	}

	var err error
	data.User = &sess.user
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
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
}

// project shows one project and the viewer's permissions in it.
//
// A visitor who is not a member gets the same 404 as for a project that
// does not exist, so the answer does not reveal which projects exist.
func (s *server) project(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentSession(r).user
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var held permissions.Set
	if !user.IsOwner {
		names, err := s.store.MemberPermissions(ctx, id, user.ID)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if held, err = permissions.ParseSet(names); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	perms := permissions.Effective(user.IsOwner, held)

	p, err := s.store.Project(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	data := struct {
		Project       store.Project
		IsOwner       bool
		Permissions   []permissions.Permission
		CanSeeMembers bool
		Members       []memberView
	}{
		Project:       p,
		IsOwner:       user.IsOwner,
		Permissions:   perms.List(),
		CanSeeMembers: perms.Has(permissions.PermMembersView),
	}
	if data.CanSeeMembers {
		if data.Members, err = s.memberViews(r, id); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, http.StatusOK, "project.html", data)
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
