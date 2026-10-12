package web

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// settingsPage is the data for settings.html.
type settingsPage struct {
	User    *store.User
	Project store.Project
	Name    string // the name field, as sent when there is an error
	Saved   bool
	Error   string // for the rename form
	// DeleteError is for the delete form, shown only to the owner.
	DeleteError string
}

// settings shows the project's settings. It needs project:edit.
func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	if !perms.Has(permissions.PermProjectEdit) {
		http.Error(w, "You cannot change this project's settings.", http.StatusForbidden)
		return
	}
	s.render(w, http.StatusOK, "settings.html", settingsPage{
		User:    &currentSession(r).user,
		Project: p,
		Name:    p.Name,
		Saved:   r.URL.Query().Get("saved") == "1",
	})
}

// saveSettings renames the project. It needs project:edit.
func (s *server) saveSettings(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	if !perms.Has(permissions.PermProjectEdit) {
		http.Error(w, "You cannot change this project's settings.", http.StatusForbidden)
		return
	}
	if !parseForm(w, r) {
		return
	}
	name, msg := projectName(r.PostFormValue("name"))
	if msg != "" {
		s.render(w, http.StatusBadRequest, "settings.html", settingsPage{
			User: &currentSession(r).user, Project: p, Name: r.PostFormValue("name"), Error: msg,
		})
		return
	}
	if err := s.store.RenameProject(r.Context(), p.ID, name); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"/settings?saved=1", http.StatusSeeOther)
}

// deleteProject deletes the project and everything in it. Only the
// instance owner may, after typing the project's name to confirm.
func (s *server) deleteProject(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.openProject(w, r)
	if !ok {
		return
	}
	user := currentSession(r).user
	if !user.IsOwner {
		http.Error(w, "Only the instance owner can delete a project.", http.StatusForbidden)
		return
	}
	if !parseForm(w, r) {
		return
	}
	if r.PostFormValue("confirm") != p.Name {
		s.render(w, http.StatusBadRequest, "settings.html", settingsPage{
			User: &user, Project: p, Name: p.Name,
			DeleteError: "Not deleted: type the project's name exactly as shown.",
		})
		return
	}
	if err := s.store.DeleteProject(r.Context(), p.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// projectName trims and checks a project name. msg explains a problem,
// or is empty.
func projectName(raw string) (name, msg string) {
	name = strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(name); n == 0 || n > maxProjectName {
		return name, "A project name must be 1 to 100 characters."
	}
	return name, ""
}
