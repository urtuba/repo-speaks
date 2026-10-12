package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// permissionHints explain each permission in plain words on the page.
var permissionHints = map[permissions.Permission]string{
	permissions.PermResourcesView: "Read the project's code and documents.",
	permissions.PermResourcesEdit: "Add and remove repositories and documents.",
	permissions.PermAgentsEdit:    "Create and change agents, including their instructions.",
	permissions.PermChatsView:     "Read every chat in the project, not only their own.",
	permissions.PermMembersView:   "See who is in the project.",
	permissions.PermMembersEdit:   "Invite, change and remove members.",
	permissions.PermProjectEdit:   "Rename the project and change its settings.",
}

// checkbox is one permission on the member page.
type checkbox struct {
	Name     permissions.Permission
	Hint     string
	Checked  bool
	Allowed  bool   // the editor holds it, so may give it
	Requires string // space-separated names the page ticks and locks with it
}

// memberPage is the data for member.html.
type memberPage struct {
	User       *store.User
	Project    store.Project
	Member     store.User
	Role       string // the preset the permissions match, or "custom"
	Checkboxes []checkbox
	Error      string
}

// memberTarget loads the member in the URL and checks that the viewer may
// change them: not themselves, and not someone above them. It returns
// false when it has already answered.
func (s *server) memberTarget(w http.ResponseWriter, r *http.Request, p store.Project, editor permissions.Set) (store.User, permissions.Set, bool) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.User{}, nil, false
	}
	if id == currentSession(r).user.ID {
		http.Error(w, "You cannot change your own permissions. Ask another manager or the owner.", http.StatusForbidden)
		return store.User{}, nil, false
	}
	names, err := s.store.MemberPermissions(ctx, p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.User{}, nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.User{}, nil, false
	}
	held, err := permissions.ParseSet(names)
	if err != nil {
		s.serverError(w, r, err)
		return store.User{}, nil, false
	}
	if !permissions.CanManage(editor, held) {
		http.Error(w, "You can only change members whose permissions you all hold.", http.StatusForbidden)
		return store.User{}, nil, false
	}
	member, err := s.store.UserByID(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return store.User{}, nil, false
	}
	return member, held, true
}

// memberPage shows a member's permissions as checkboxes.
func (s *server) memberPage(w http.ResponseWriter, r *http.Request) {
	p, editor, ok := s.openProject(w, r)
	if !ok {
		return
	}
	member, held, ok := s.memberTarget(w, r, p, editor)
	if !ok {
		return
	}
	s.renderMember(w, r, http.StatusOK, p, editor, member, held, "")
}

// saveMember replaces a member's permissions with the ticked ones. The
// page ticks what each permission needs, but the server checks again:
// a set that breaks a rule is refused with a message, and nothing
// changes.
func (s *server) saveMember(w http.ResponseWriter, r *http.Request) {
	p, editor, ok := s.openProject(w, r)
	if !ok {
		return
	}
	member, held, ok := s.memberTarget(w, r, p, editor)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	next, err := permissions.ParseSet(r.PostForm["permission"])
	if err != nil {
		s.renderMember(w, r, http.StatusBadRequest, p, editor, member, held, "The form had a permission this app does not know.")
		return
	}
	if err := permissions.Validate(next); err != nil {
		msg := strings.TrimSuffix(err.Error(), ": "+permissions.ErrInvalidSet.Error())
		s.renderMember(w, r, http.StatusBadRequest, p, editor, member, next, "Not saved: "+msg+".")
		return
	}
	if !permissions.CanSet(editor, held, next) {
		s.renderMember(w, r, http.StatusForbidden, p, editor, member, held, "You can only give permissions you hold yourself.")
		return
	}
	if err := s.store.SetMemberPermissions(r.Context(), p.ID, member.ID, next.Strings()); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"#members", http.StatusSeeOther)
}

// removeMember takes a member out of the project. Their chats stay, and
// their open invite links stop working.
func (s *server) removeMember(w http.ResponseWriter, r *http.Request) {
	p, editor, ok := s.openProject(w, r)
	if !ok {
		return
	}
	member, _, ok := s.memberTarget(w, r, p, editor)
	if !ok {
		return
	}
	if err := s.store.RemoveMember(r.Context(), p.ID, member.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"#members", http.StatusSeeOther)
}

// leaveProject takes the viewer out of the project.
func (s *server) leaveProject(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.openProject(w, r)
	if !ok {
		return
	}
	user := currentSession(r).user
	if user.IsOwner {
		http.Error(w, "The instance owner is not a member, so there is nothing to leave.", http.StatusBadRequest)
		return
	}
	if err := s.store.RemoveMember(r.Context(), p.ID, user.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderMember renders member.html with the checkboxes for shown.
func (s *server) renderMember(w http.ResponseWriter, r *http.Request, status int, p store.Project, editor permissions.Set, member store.User, shown permissions.Set, errMsg string) {
	page := memberPage{
		User:    &currentSession(r).user,
		Project: p,
		Member:  member,
		Role:    roleName(shown),
		Error:   errMsg,
	}
	for _, perm := range permissions.List() {
		var needs []string
		for _, n := range permissions.Requires(perm) {
			needs = append(needs, string(n))
		}
		page.Checkboxes = append(page.Checkboxes, checkbox{
			Name:     perm,
			Hint:     permissionHints[perm],
			Checked:  shown.Has(perm),
			Allowed:  editor.Has(perm),
			Requires: strings.Join(needs, " "),
		})
	}
	s.render(w, status, "member.html", page)
}

// roleName returns the preset whose permissions are exactly set, or
// "custom".
func roleName(set permissions.Set) string {
	for _, p := range permissions.Presets() {
		preset := p.Permissions()
		if preset.HasAll(set) && set.HasAll(preset) {
			return string(p)
		}
	}
	if len(set.List()) == 0 {
		return "asks only"
	}
	return "custom"
}
