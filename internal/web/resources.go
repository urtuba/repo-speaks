package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// These handlers only read and write the database. A sync is a job row
// that the jobs loop picks up; no handler runs git or touches the disk,
// so a slow clone never holds up a page.

// resourceNotices are the messages the project page shows after a
// resource action, by the notice name in the URL. A name that is not
// here shows nothing, so the URL cannot put text on the page.
var resourceNotices = map[string]string{
	"added":   "Repository added. Its first sync is queued.",
	"queued":  "Sync queued.",
	"open":    "Sync already queued.",
	"removed": "Repository removed.",
}

// resourceView is a resource as the project page shows it.
type resourceView struct {
	store.Resource
	RepoURL     string
	Ago         string // when the last good sync was, such as "5 minutes ago"
	SyncedAt    string // the same time in full
	ShortCommit string
	CommitURL   string
}

// resourceList is the data for the "resources" template: the list on
// the project page, which htmx reloads while a sync is queued or running.
type resourceList struct {
	ProjectID int64
	CanEdit   bool // resources:edit
	Items     []resourceView
	Open      bool // a sync is queued or running, so the list reloads itself
}

// loadResources loads the project's resources for display.
func (s *server) loadResources(r *http.Request, projectID int64, canEdit bool) (resourceList, error) {
	list, err := s.store.Resources(r.Context(), projectID)
	if err != nil {
		return resourceList{}, err
	}
	now := s.now()
	out := resourceList{ProjectID: projectID, CanEdit: canEdit}
	for _, res := range list {
		v := resourceView{Resource: res, RepoURL: githubURL(res.Owner, res.Repo)}
		if !res.LastSyncedAt.IsZero() {
			v.Ago = ago(res.LastSyncedAt, now)
			v.SyncedAt = res.LastSyncedAt.Format("2 Jan 2006, 15:04 UTC")
		}
		if len(res.LastCommit) >= 7 {
			v.ShortCommit = res.LastCommit[:7]
			v.CommitURL = v.RepoURL + "/commit/" + res.LastCommit
		}
		if res.OpenJob != "" {
			out.Open = true
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

// resourcesFragment renders the resource list alone, for htmx to swap
// in while a sync is open. It needs resources:view.
func (s *server) resourcesFragment(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	if !perms.Has(permissions.PermResourcesView) {
		http.Error(w, "You cannot see this project's repositories.", http.StatusForbidden)
		return
	}
	list, err := s.loadResources(r, p.ID, perms.Has(permissions.PermResourcesEdit))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "resources", list)
}

// addResource adds a GitHub repository to the project and queues its
// first sync. It needs resources:edit.
func (s *server) addResource(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	if !perms.Has(permissions.PermResourcesEdit) {
		http.Error(w, "You cannot add repositories to this project.", http.StatusForbidden)
		return
	}
	if !parseForm(w, r) {
		return
	}
	// The form is shown again after an error, without the token.
	page := projectPage{RepoInput: r.PostFormValue("repo"), BranchInput: r.PostFormValue("branch")}
	owner, repo, msg := parseRepo(page.RepoInput)
	var branch, token string
	if msg == "" {
		branch, msg = parseBranch(page.BranchInput)
	}
	if msg == "" {
		token, msg = parseToken(r.PostFormValue("token"), false)
	}
	if msg != "" {
		page.ResourceError = msg
		s.renderProjectPage(w, r, http.StatusBadRequest, p, perms, page)
		return
	}
	_, err := s.store.AddResource(r.Context(), store.NewResource{
		ProjectID: p.ID, Owner: owner, Repo: repo, Branch: branch, Token: token,
	}, s.now())
	if errors.Is(err, store.ErrResourceExists) {
		page.ResourceError = "The project already has this repository with this branch."
		s.renderProjectPage(w, r, http.StatusConflict, p, perms, page)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"?notice=added#resources", http.StatusSeeOther)
}

// openResource loads the project and the resource in the URL, and
// checks that the viewer has resources:edit. A resource of another
// project gets 404, like a missing one. It returns false when it has
// already answered.
func (s *server) openResource(w http.ResponseWriter, r *http.Request) (store.Project, store.Resource, bool) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return store.Project{}, store.Resource{}, false
	}
	if !perms.Has(permissions.PermResourcesEdit) {
		http.Error(w, "You cannot change this project's repositories.", http.StatusForbidden)
		return store.Project{}, store.Resource{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("resource"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Project{}, store.Resource{}, false
	}
	res, err := s.store.Resource(r.Context(), p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Project{}, store.Resource{}, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.Project{}, store.Resource{}, false
	}
	return p, res, true
}

// syncResource queues a sync of the resource, unless one is already
// queued or running.
func (s *server) syncResource(w http.ResponseWriter, r *http.Request) {
	p, res, ok := s.openResource(w, r)
	if !ok {
		return
	}
	queued, err := s.store.QueueSync(r.Context(), res.ID, s.now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	notice := "queued"
	if !queued {
		notice = "open"
	}
	http.Redirect(w, r, projectPath(p.ID)+"?notice="+notice+"#resources", http.StatusSeeOther)
}

// removeResource removes the resource from the project. The jobs loop
// deletes its clone later.
func (s *server) removeResource(w http.ResponseWriter, r *http.Request) {
	p, res, ok := s.openResource(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteResource(r.Context(), p.ID, res.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"?notice=removed#resources", http.StatusSeeOther)
}

// resourcePage is the data for resource.html.
type resourcePage struct {
	User     *store.User
	Project  store.Project
	Resource store.Resource
	Saved    string // "set" or "cleared" after a change
	Error    string
}

// resourcePageView shows a resource's token state, with forms to set,
// replace or clear the token. The token itself is never shown.
func (s *server) resourcePageView(w http.ResponseWriter, r *http.Request) {
	p, res, ok := s.openResource(w, r)
	if !ok {
		return
	}
	saved := r.URL.Query().Get("saved")
	if saved != "set" && saved != "cleared" {
		saved = ""
	}
	s.render(w, http.StatusOK, "resource.html", resourcePage{
		User: &currentSession(r).user, Project: p, Resource: res, Saved: saved,
	})
}

// setResourceToken sets or replaces the resource's token.
func (s *server) setResourceToken(w http.ResponseWriter, r *http.Request) {
	p, res, ok := s.openResource(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	token, msg := parseToken(r.PostFormValue("token"), true)
	if msg != "" {
		s.render(w, http.StatusBadRequest, "resource.html", resourcePage{
			User: &currentSession(r).user, Project: p, Resource: res, Error: msg,
		})
		return
	}
	if err := s.store.SetResourceToken(r.Context(), p.ID, res.ID, token); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, resourcePath(p.ID, res.ID)+"?saved=set", http.StatusSeeOther)
}

// clearResourceToken removes the resource's token.
func (s *server) clearResourceToken(w http.ResponseWriter, r *http.Request) {
	p, res, ok := s.openResource(w, r)
	if !ok {
		return
	}
	if err := s.store.SetResourceToken(r.Context(), p.ID, res.ID, ""); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, resourcePath(p.ID, res.ID)+"?saved=cleared", http.StatusSeeOther)
}

// resourcePath is the URL of a resource's page.
func resourcePath(projectID, resourceID int64) string {
	return projectPath(projectID) + "/resources/" + strconv.FormatInt(resourceID, 10)
}

// githubURL is a repository's page on GitHub.
func githubURL(owner, repo string) string {
	return "https://github.com/" + owner + "/" + repo
}

// The names a builder may enter. They follow GitHub's rules, and none
// may start with "-", so git cannot read one as an option. The sync
// checks the same rules again before it runs git.
var (
	ownerRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,99}$`)
	branchRE = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,254}$`)
	tokenRE  = regexp.MustCompile(`^[A-Za-z0-9_]{20,255}$`)
)

// parseRepo reads a GitHub repository in one of the forms a builder may
// paste: owner/repo, github.com/owner/repo or
// https://github.com/owner/repo, with or without .git at the end. msg
// explains a problem, or is empty.
func parseRepo(raw string) (owner, repo, msg string) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "https://github.com/"):
		s = strings.TrimPrefix(s, "https://github.com/")
	case strings.HasPrefix(s, "github.com/"):
		s = strings.TrimPrefix(s, "github.com/")
	case strings.Contains(s, "://"):
		return "", "", "Only GitHub repositories are supported, at https://github.com/owner/repo."
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", "Enter the repository as owner/repo, such as urtuba/repo-speaks."
	}
	if !ownerRE.MatchString(owner) {
		return "", "", "The owner name may use letters, digits and -, and may not start with -."
	}
	if !repoRE.MatchString(repo) || repo == "." || strings.Contains(repo, "..") {
		return "", "", "The repository name may use letters, digits and . _ -, and may not start with -."
	}
	return owner, repo, ""
}

// parseBranch checks an optional branch name. Empty means the
// repository's default branch.
func parseBranch(raw string) (branch, msg string) {
	branch = strings.TrimSpace(raw)
	if branch == "" {
		return "", ""
	}
	if !branchRE.MatchString(branch) || strings.Contains(branch, "..") {
		return "", "The branch name may use letters, digits and . _ / -, may not start with - or /, and may not contain two dots in a row."
	}
	return branch, ""
}

// parseToken checks a GitHub token. GitHub tokens use only letters,
// digits and underscores. An empty token is fine unless required.
func parseToken(raw string, required bool) (token, msg string) {
	token = strings.TrimSpace(raw)
	if token == "" {
		if required {
			return "", "Enter a token."
		}
		return "", ""
	}
	if !tokenRE.MatchString(token) {
		return "", "That does not look like a GitHub token: it should be letters, digits and underscores, such as github_pat_..."
	}
	return token, ""
}

// ago says roughly how long before now t was, such as "5 minutes ago".
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return count(int(d/time.Minute), "minute") + " ago"
	case d < 24*time.Hour:
		return count(int(d/time.Hour), "hour") + " ago"
	default:
		return count(int(d/(24*time.Hour)), "day") + " ago"
	}
}

// count writes n with its unit, such as "1 hour" or "3 hours".
func count(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
