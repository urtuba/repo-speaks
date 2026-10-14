package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/urtuba/repo-speaks/internal/llm"
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

	// The sync form: the fields' values, as sent when there is an error.
	Intervals    []syncInterval
	SyncInterval int
	MaxFileMB    string
	SyncSaved    bool
	SyncError    string

	// The models card. The add form's fields are as sent when there is
	// an error; the key is never sent back.
	Models       []store.Model
	ModelNotice  string
	ModelError   string
	ModelName    string
	ModelBaseURL string
	ModelID      string
}

// syncInterval is one choice for how often a project's resources sync.
type syncInterval struct {
	Hours int // 0 turns the timer off
	Label string
}

// syncIntervals are the choices, in the order the page lists them.
var syncIntervals = []syncInterval{
	{0, "Off: only when added or on Sync now"},
	{1, "Every hour"},
	{6, "Every 6 hours"},
	{24, "Every 24 hours"},
}

// The file size limit a project may set, in MB.
const (
	minFileMB = 1
	maxFileMB = 100
)

// newSettingsPage fills the settings page with the project's values.
func newSettingsPage(r *http.Request, p store.Project) settingsPage {
	return settingsPage{
		User:         &currentSession(r).user,
		Project:      p,
		Name:         p.Name,
		Intervals:    syncIntervals,
		SyncInterval: p.SyncIntervalHours,
		MaxFileMB:    strconv.FormatInt(p.MaxFileBytes>>20, 10),
		ModelBaseURL: llm.DefaultBaseURL,
	}
}

// renderSettings loads the project's models into page and renders the
// settings page.
func (s *server) renderSettings(w http.ResponseWriter, r *http.Request, status int, page settingsPage) {
	models, err := s.store.Models(r.Context(), page.Project.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	page.Models = models
	s.render(w, status, "settings.html", page)
}

// openSettings loads the project in the URL and checks that the viewer
// has project:edit. It returns false when it has already answered.
func (s *server) openSettings(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return store.Project{}, false
	}
	if !perms.Has(permissions.PermProjectEdit) {
		http.Error(w, "You cannot change this project's settings.", http.StatusForbidden)
		return store.Project{}, false
	}
	return p, true
}

// settings shows the project's settings. It needs project:edit.
func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.openSettings(w, r)
	if !ok {
		return
	}
	page := newSettingsPage(r, p)
	saved := r.URL.Query().Get("saved")
	page.Saved = saved == "1"
	page.SyncSaved = saved == "sync"
	page.ModelNotice = modelNotices[saved]
	s.renderSettings(w, r, http.StatusOK, page)
}

// saveSettings renames the project. It needs project:edit.
func (s *server) saveSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.openSettings(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	name, msg := projectName(r.PostFormValue("name"))
	if msg != "" {
		page := newSettingsPage(r, p)
		page.Name, page.Error = r.PostFormValue("name"), msg
		s.renderSettings(w, r, http.StatusBadRequest, page)
		return
	}
	if err := s.store.RenameProject(r.Context(), p.ID, name); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"/settings?saved=1", http.StatusSeeOther)
}

// saveSyncSettings sets how often the project's resources sync and the
// largest file a sync keeps. It needs project:edit. The new file limit
// applies from the next sync.
func (s *server) saveSyncSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.openSettings(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	page := newSettingsPage(r, p)
	page.MaxFileMB = strings.TrimSpace(r.PostFormValue("max_file_mb"))
	hours, err := strconv.Atoi(r.PostFormValue("sync_interval"))
	known := err == nil && slices.ContainsFunc(syncIntervals, func(i syncInterval) bool { return i.Hours == hours })
	if !known {
		page.SyncError = "Pick one of the sync intervals in the list."
		s.renderSettings(w, r, http.StatusBadRequest, page)
		return
	}
	page.SyncInterval = hours
	mb, err := strconv.ParseInt(page.MaxFileMB, 10, 64)
	if err != nil || mb < minFileMB || mb > maxFileMB {
		page.SyncError = "The file size limit must be a whole number from 1 to 100 MB."
		s.renderSettings(w, r, http.StatusBadRequest, page)
		return
	}
	if err := s.store.SetSyncSettings(r.Context(), p.ID, hours, mb<<20); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"/settings?saved=sync", http.StatusSeeOther)
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
		page := newSettingsPage(r, p)
		page.DeleteError = "Not deleted: type the project's name exactly as shown."
		s.renderSettings(w, r, http.StatusBadRequest, page)
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
