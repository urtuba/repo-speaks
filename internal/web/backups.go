package web

import (
	"fmt"
	"net/http"

	"github.com/urtuba/repo-speaks/internal/store"
)

// The Backups page belongs to the owner. "Back up now" only adds a job
// row; the jobs loop makes the backup, so a slow upload never holds up
// a page. The targets and the passphrase come from the environment, so
// the page has nothing to change and shows no secret.

// backupNotices are the messages the Backups page shows after "Back up
// now", by the notice name in the URL. A name that is not here shows
// nothing.
var backupNotices = map[string]string{
	"queued": "Backup queued.",
	"open":   "A backup is already queued or running.",
}

// recentBackups is how many backups the page lists.
const recentBackups = 20

// backupView is a backup as the page shows it.
type backupView struct {
	store.Backup
	At   string // when it started, in full
	Ago  string
	Size string
}

// backupList is the data for the "backups" template: the list on the
// Backups page, which htmx reloads while a backup is queued or running.
type backupList struct {
	Open  string // "queued" or "running" while a backup job is open
	Items []backupView
}

// backupsPage is the data for backups.html.
type backupsPage struct {
	User    *store.User
	On      bool
	Targets []string
	Notice  string
	List    backupList
}

// ownerOnly answers 403 to anyone but the owner, and reports whether
// the request may go on.
func ownerOnly(w http.ResponseWriter, r *http.Request) bool {
	if !currentSession(r).user.IsOwner {
		http.Error(w, "Only the instance owner can see backups.", http.StatusForbidden)
		return false
	}
	return true
}

// loadBackups loads the open backup job and the recent backups.
func (s *server) loadBackups(r *http.Request) (backupList, error) {
	open, err := s.store.OpenBackupJob(r.Context())
	if err != nil {
		return backupList{}, err
	}
	list, err := s.store.Backups(r.Context(), recentBackups)
	if err != nil {
		return backupList{}, err
	}
	now := s.now()
	out := backupList{Open: open}
	for _, b := range list {
		out.Items = append(out.Items, backupView{
			Backup: b,
			At:     b.StartedAt.Format("2 Jan 2006, 15:04 UTC"),
			Ago:    ago(b.StartedAt, now),
			Size:   sizeText(b.Size),
		})
	}
	return out, nil
}

// backups shows whether backups are on, where they go, and the recent
// ones.
func (s *server) backups(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r) {
		return
	}
	list, err := s.loadBackups(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "backups.html", backupsPage{
		User:    &currentSession(r).user,
		On:      s.backup.On(),
		Targets: s.backup.Targets(),
		Notice:  backupNotices[r.URL.Query().Get("notice")],
		List:    list,
	})
}

// backupsFragment renders the list alone, for htmx to swap in while a
// backup is open.
func (s *server) backupsFragment(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r) {
		return
	}
	list, err := s.loadBackups(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "backups", list)
}

// queueBackup queues a backup, unless one is already queued or running.
func (s *server) queueBackup(w http.ResponseWriter, r *http.Request) {
	if !ownerOnly(w, r) {
		return
	}
	if !s.backup.On() {
		http.Error(w, "Backups are off. Set a backup target and a passphrase in the server's environment.", http.StatusBadRequest)
		return
	}
	queued, err := s.store.QueueBackup(r.Context(), s.now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	notice := "queued"
	if !queued {
		notice = "open"
	}
	http.Redirect(w, r, "/backups?notice="+notice, http.StatusSeeOther)
}

// sizeText writes a number of bytes for people, such as "4.2 MB".
func sizeText(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, prefix := float64(n)/unit, 0
	for value >= unit && prefix < 3 {
		value /= unit
		prefix++
	}
	return fmt.Sprintf("%.1f %cB", value, "kMGT"[prefix])
}
