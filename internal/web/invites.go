package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/permissions"
	"github.com/urtuba/repo-speaks/internal/store"
)

// createInvite makes an invite link for the project. The creator needs
// members:edit and must hold every permission of the preset. The link
// is shown once: only its hash is stored.
func (s *server) createInvite(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	if !perms.Has(permissions.PermMembersEdit) {
		http.Error(w, "You cannot invite members to this project.", http.StatusForbidden)
		return
	}
	if !parseForm(w, r) {
		return
	}
	preset := permissions.Preset(r.PostFormValue("preset"))
	if !permissions.CanInvite(perms, preset) {
		s.renderProject(w, r, http.StatusForbidden, p, perms, "You can only invite with a role whose permissions you all hold.")
		return
	}

	sess := currentSession(r)
	now := s.now()
	token, hash := auth.NewToken()
	expires := auth.InviteExpiry(now)
	if err := s.store.CreateInvite(r.Context(), p.ID, hash, string(preset), sess.user.ID, now, expires); err != nil {
		s.serverError(w, r, err)
		return
	}
	// The page holds a working link, so no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "invite_created.html", struct {
		User      *store.User
		Project   store.Project
		Preset    permissions.Preset
		Link      string
		ExpiresAt time.Time
	}{&sess.user, p, preset, linkBase(r) + invitePath(token), expires})
}

// revokeInvite stops an open invite link from working.
func (s *server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	p, perms, ok := s.openProject(w, r)
	if !ok {
		return
	}
	if !perms.Has(permissions.PermMembersEdit) {
		http.Error(w, "You cannot manage invites in this project.", http.StatusForbidden)
		return
	}
	inviteID, err := strconv.ParseInt(r.PathValue("invite"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.store.RevokeInvite(r.Context(), p.ID, inviteID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"#invites", http.StatusSeeOther)
}

// invitePage is the data for invite.html.
type invitePage struct {
	// State picks what the page shows: "dead", "member", "owner",
	// "confirm" (logged in, can join) or "signup" (new account).
	State    string
	Invite   store.Invite
	User     *store.User
	Path     string // the invite's own URL, for forms and links
	Username string
	Error    string
}

// showInvite shows what an invite link offers. A visitor who is not
// logged in gets a form for a new account; a logged-in visitor confirms.
func (s *server) showInvite(w http.ResponseWriter, r *http.Request) {
	page, ok := s.inviteState(w, r)
	if !ok {
		return
	}
	status := http.StatusOK
	if page.State == "dead" {
		status = http.StatusNotFound
	}
	s.renderInvite(w, status, page)
}

// acceptInvite joins the project through an invite link: as the
// logged-in account, or with a new account made from the form.
func (s *server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	page, ok := s.inviteState(w, r)
	if !ok {
		return
	}
	switch page.State {
	case "dead":
		s.renderInvite(w, http.StatusNotFound, page)
		return
	case "member", "owner":
		s.renderInvite(w, http.StatusConflict, page)
		return
	}

	a := store.Acceptance{
		Invite:      page.Invite,
		Permissions: permissions.Preset(page.Invite.Preset).Permissions().Strings(),
		Now:         s.now(),
	}
	if page.State == "confirm" {
		a.UserID = page.User.ID
	} else {
		username, password, msg, ok := newAccountForm(w, r)
		if !ok {
			return
		}
		page.Username = username
		if msg != "" {
			page.Error = msg
			s.renderInvite(w, http.StatusBadRequest, page)
			return
		}
		hash, err := auth.HashPassword(password)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		a.Username, a.PasswordHash = username, hash
	}

	userID, err := s.store.AcceptInvite(r.Context(), a)
	switch {
	case errors.Is(err, store.ErrInviteGone):
		// Someone else used the link, or it expired, while this request ran.
		page.State = "dead"
		s.renderInvite(w, http.StatusNotFound, page)
		return
	case errors.Is(err, store.ErrAlreadyMember):
		page.State = "member"
		s.renderInvite(w, http.StatusConflict, page)
		return
	case errors.Is(err, store.ErrUsernameTaken):
		page.Error = "That username is taken. Pick another one, or log in first if it is yours."
		s.renderInvite(w, http.StatusBadRequest, page)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if page.State == "signup" {
		if err := s.startSession(w, r, userID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	http.Redirect(w, r, projectPath(page.Invite.ProjectID), http.StatusSeeOther)
}

// inviteState looks up the invite in the URL and decides what the
// visitor may do with it. It returns false when it has already answered.
func (s *server) inviteState(w http.ResponseWriter, r *http.Request) (invitePage, bool) {
	ctx := r.Context()
	token := r.PathValue("token")
	page := invitePage{Path: invitePath(token)}
	sess := currentSession(r)
	if sess != nil {
		page.User = &sess.user
	}

	inv, usable, err := s.usableInvite(ctx, token)
	if err != nil {
		s.serverError(w, r, err)
		return page, false
	}
	page.Invite = inv
	switch {
	case !usable:
		page.State = "dead"
	case sess == nil:
		page.State = "signup"
	case sess.user.IsOwner:
		page.State = "owner"
	default:
		_, err := s.store.MemberPermissions(ctx, inv.ProjectID, sess.user.ID)
		switch {
		case err == nil:
			page.State = "member"
		case errors.Is(err, store.ErrNotFound):
			page.State = "confirm"
		default:
			s.serverError(w, r, err)
			return page, false
		}
	}
	// These pages are reached with a working link in the URL.
	w.Header().Set("Cache-Control", "no-store")
	return page, true
}

// usableInvite finds the invite for a link token and reports whether it
// still works: not used, not revoked, not expired, and its creator still
// holds every permission of the preset. A creator who lost rights, left
// the project or was deleted takes their open links with them.
func (s *server) usableInvite(ctx context.Context, token string) (store.Invite, bool, error) {
	inv, err := s.store.InviteByHash(ctx, auth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return store.Invite{}, false, nil
	}
	if err != nil {
		return store.Invite{}, false, err
	}
	if !inv.Open(s.now()) || inv.CreatedBy == 0 {
		return inv, false, nil
	}
	creator, err := s.store.UserByID(ctx, inv.CreatedBy)
	if errors.Is(err, store.ErrNotFound) {
		return inv, false, nil
	}
	if err != nil {
		return inv, false, err
	}
	perms, err := s.projectPerms(ctx, creator, inv.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		return inv, false, nil
	}
	if err != nil {
		return inv, false, err
	}
	return inv, permissions.CanInvite(perms, permissions.Preset(inv.Preset)), nil
}

// renderInvite renders invite.html.
func (s *server) renderInvite(w http.ResponseWriter, status int, page invitePage) {
	s.render(w, status, "invite.html", page)
}

// invitePath is the path of an invite link.
func invitePath(token string) string {
	return "/invites/" + token
}

// linkBase returns the scheme and host the visitor used, such as
// https://repospeaks.example.com, to build a full link. Behind a reverse
// proxy, X-Forwarded-Proto tells whether the visitor used HTTPS. The
// link is only shown to the person who asked for it.
func linkBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
