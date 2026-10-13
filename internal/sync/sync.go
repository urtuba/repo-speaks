// Package sync brings one resource up to date with GitHub. It checks the
// repository's size, clones the latest commit with the git command and
// trims the clone. It runs no loop: the jobs package calls it.
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/urtuba/repo-speaks/internal/store"
)

// maxRepoKB is the largest repository a sync clones: 1 GB, in KB, the
// unit of the size GitHub reports.
const maxRepoKB = 1 << 20

// apiTimeout limits the size check.
const apiTimeout = 30 * time.Second

// Syncer clones resources into <data>/repos/<project>/<resource>/<folder>.
// Each sync makes a new folder, so a question reading the old one is
// never cut off halfway.
type Syncer struct {
	store    *store.Store
	reposDir string // absolute, because git runs in another folder
	runner   Runner
	path     string // the PATH git runs with
	client   *http.Client
	now      func() time.Time

	// Where GitHub is. Tests point these at local servers. They are not
	// settings, so nobody can make the server send a token elsewhere.
	apiBase string // the REST API
	gitBase string // where repositories are cloned from
	caFile  string // a certificate authority git trusts, for tests only
}

// New returns a Syncer that keeps clones under dataDir and runs the git
// command.
func New(st *store.Store, dataDir string) (*Syncer, error) {
	abs, err := filepath.Abs(filepath.Join(dataDir, "repos"))
	if err != nil {
		return nil, fmt.Errorf("clone folder: %w", err)
	}
	return &Syncer{
		store:    st,
		reposDir: abs,
		runner:   execRunner{program: "git"},
		path:     os.Getenv("PATH"),
		client:   &http.Client{Timeout: apiTimeout},
		now:      time.Now,
		apiBase:  "https://api.github.com",
		gitBase:  "https://github.com",
	}, nil
}

// Error is a failed sync. Its message is safe to save and show: it never
// holds the token.
type Error struct {
	msg       string
	permanent bool
}

func (e *Error) Error() string { return e.msg }

// Permanent reports whether trying again cannot help, as with a
// repository over the size limit or one that does not exist.
func (e *Error) Permanent() bool { return e.permanent }

// permanentError marks a failure that trying again cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// permanentf is fmt.Errorf for a failure that trying again cannot fix.
func permanentf(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

// maxErrorLen caps an error message, which pages show.
const maxErrorLen = 1000

// cleanError turns err into an *Error with the token taken out, both as
// typed and as it appears in the auth header. Git does not print either,
// but an error message is saved and shown, so it must not depend on that.
func cleanError(err error, token string) *Error {
	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, basicAuth(token), "[hidden]")
		msg = strings.ReplaceAll(msg, token, "[hidden]")
	}
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen] + "..."
	}
	_, permanent := errors.AsType[permanentError](err)
	return &Error{msg: msg, permanent: permanent}
}

// Sync brings the resource up to date:
//
//  1. ask GitHub for the repository's size, and stop when it is too large;
//  2. clone the latest commit into a new folder;
//  3. record the commit, then delete .git and every file over the
//     project's limit;
//  4. point the resource at the new folder;
//  5. delete older folders, but keep the one before, which a question
//     may still read.
//
// On failure it returns an *Error and leaves the current clone alone.
func (s *Syncer) Sync(ctx context.Context, resourceID int64) error {
	src, err := s.store.SyncSource(ctx, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		return &Error{msg: "the resource was removed", permanent: true}
	}
	if err != nil {
		return cleanError(err, "")
	}
	if err := s.sync(ctx, src); err != nil {
		return cleanError(err, src.Token)
	}
	return nil
}

func (s *Syncer) sync(ctx context.Context, src store.SyncSource) error {
	if src.Kind != store.KindGitHubRepo {
		return permanentf("resources of kind %q cannot sync", src.Kind)
	}
	if err := checkNames(src.Owner, src.Repo, src.Branch); err != nil {
		return err
	}
	if err := s.checkSize(ctx, src); err != nil {
		return err
	}

	dir := s.resourceDir(src.ProjectID, src.ResourceID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("make the clone folder: %w", err)
	}
	name := strconv.FormatInt(s.now().UnixNano(), 10)
	clone := filepath.Join(dir, name)
	// Mkdir fails when the folder exists, so a sync only ever deletes a
	// folder it made itself.
	if err := os.Mkdir(clone, 0o750); err != nil {
		return fmt.Errorf("make the clone folder: %w", err)
	}
	commit, err := s.fetch(ctx, src, clone)
	if err != nil {
		os.RemoveAll(clone)
		return err
	}
	if err := s.store.SaveSync(ctx, src.ResourceID, name, commit, s.now()); err != nil {
		os.RemoveAll(clone)
		if errors.Is(err, store.ErrNotFound) {
			return permanentf("the resource was removed during the sync")
		}
		return err
	}
	removeOld(dir, name, src.CloneDir)
	return nil
}

// resourceDir is the folder that holds a resource's clones.
func (s *Syncer) resourceDir(projectID, resourceID int64) string {
	return filepath.Join(s.reposDir, strconv.FormatInt(projectID, 10), strconv.FormatInt(resourceID, 10))
}

// commitRE matches a full commit ID: SHA-1, or SHA-256 in repositories
// that use it.
var commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// fetch clones the latest commit into dir and trims the clone. It
// returns the commit ID.
func (s *Syncer) fetch(ctx context.Context, src store.SyncSource, dir string) (string, error) {
	// git reads nothing from HOME (the env turns user config off), but
	// it must point somewhere: an empty folder of its own.
	home, err := os.MkdirTemp("", "repo-speaks-git-")
	if err != nil {
		return "", fmt.Errorf("make a home folder for git: %w", err)
	}
	defer os.RemoveAll(home)

	args := []string{"-c", "core.symlinks=false", "clone", "--depth", "1", "--single-branch", "--no-tags"}
	if src.Branch != "" {
		args = append(args, "--branch="+src.Branch)
	}
	// "--" ends the options, so nothing after it can pass as one.
	args = append(args, "--", s.cloneURL(src.Owner, src.Repo), dir)
	if _, err := s.runner.Run(ctx, home, s.env(home, src.Token), args...); err != nil {
		return "", fmt.Errorf("git clone: %w", err)
	}

	out, err := s.runner.Run(ctx, dir, s.env(home, ""), "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	commit := strings.TrimSpace(string(out))
	if !commitRE.MatchString(commit) {
		return "", errors.New("git rev-parse did not print a commit ID")
	}

	// The history is not needed. Without .git, nothing in the folder can
	// act as git settings either.
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("delete .git: %w", err)
	}
	if err := removeLarge(dir, src.MaxFileBytes); err != nil {
		return "", fmt.Errorf("delete large files: %w", err)
	}
	return commit, nil
}

// cloneURL builds the URL git clones from. Only checked names go into it.
func (s *Syncer) cloneURL(owner, repo string) string {
	return s.gitBase + "/" + owner + "/" + repo + ".git"
}

// checkSize asks GitHub for the repository's size before anything is
// downloaded. Its answer also explains a missing repository or a bad
// token more clearly than git does.
func (s *Syncer) checkSize(ctx context.Context, src store.SyncSource) error {
	name := src.Owner + "/" + src.Repo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase+"/repos/"+name, nil)
	if err != nil {
		return fmt.Errorf("size check: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "repo-speaks")
	if src.Token != "" {
		req.Header.Set("Authorization", "Bearer "+src.Token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("size check: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return permanentf("repository %s not found, or the token has no access to it", name)
	case http.StatusUnauthorized:
		return permanentf("the token was refused: it may have expired")
	case http.StatusForbidden, http.StatusTooManyRequests:
		return fmt.Errorf("size check refused with %s, maybe because of the GitHub rate limit", resp.Status)
	default:
		return fmt.Errorf("size check failed with %s", resp.Status)
	}
	var repo struct {
		Size int64 `json:"size"` // in KB
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&repo); err != nil {
		return fmt.Errorf("size check: %w", err)
	}
	if repo.Size > maxRepoKB {
		return permanentf("repository %s is %.1f GB, over the 1 GB limit", name, float64(repo.Size)/(1<<20))
	}
	return nil
}

// removeLarge deletes every file in dir larger than maxBytes. WalkDir
// does not follow links, so it never leaves dir.
func removeLarge(dir string, maxBytes int64) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxBytes {
			return os.Remove(path)
		}
		return nil
	})
}

// removeOld deletes a resource's clone folders except current and
// previous: a question that started before this sync may still read
// previous. A folder it cannot delete stays until the next sync.
func removeOld(dir, current, previous string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == current || e.Name() == previous {
			continue
		}
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// RemoveDeleted deletes the clones of resources that no longer exist,
// after a builder removed a repository or the owner deleted a project.
// It leaves alone anything whose name is not an ID: a sync did not make it.
func (s *Syncer) RemoveDeleted(ctx context.Context) error {
	projects, err := os.ReadDir(s.reposDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove deleted clones: %w", err)
	}
	var errs []error
	for _, p := range projects {
		projectID, ok := parseID(p.Name())
		if !ok || !p.IsDir() {
			continue
		}
		projectDir := filepath.Join(s.reposDir, p.Name())
		resources, err := os.ReadDir(projectDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		kept := 0
		for _, r := range resources {
			resourceID, ok := parseID(r.Name())
			if !ok || !r.IsDir() {
				kept++
				continue
			}
			exists, err := s.store.ResourceExists(ctx, projectID, resourceID)
			if err != nil {
				return fmt.Errorf("remove deleted clones: %w", err)
			}
			if exists {
				kept++
				continue
			}
			if err := os.RemoveAll(filepath.Join(projectDir, r.Name())); err != nil {
				errs = append(errs, err)
			}
		}
		if kept == 0 {
			// Remove only deletes an empty folder, so a sync that has
			// just made a resource folder here keeps it.
			os.Remove(projectDir)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove deleted clones: %w", err)
	}
	return nil
}

// parseID reads a folder name made from an ID. It accepts only the form
// strconv.FormatInt writes, so "007" or "+7" are not IDs.
func parseID(name string) (int64, bool) {
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != name {
		return 0, false
	}
	return id, true
}
