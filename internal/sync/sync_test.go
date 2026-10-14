package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/store"
)

var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

const (
	testToken  = "github_pat_11AAAAAAA0secretvalue_wxyz"
	testCommit = "0123456789abcdef0123456789abcdef01234567"
)

// fakeGitHub answers the size check for urtuba/demo.
type fakeGitHub struct {
	mu     gosync.Mutex
	status int
	size   int64 // in KB
	calls  int
	auth   string // the Authorization header of the last request
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	g.auth = r.Header.Get("Authorization")
	if r.URL.Path != "/repos/urtuba/demo" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(g.status)
	// A careless server could echo the token; the sync must not pass it on.
	fmt.Fprintf(w, `{"size": %d, "note": %q}`, g.size, g.auth)
}

// fakeRunner stands in for git. A clone writes files into the target
// folder; rev-parse prints commit.
type fakeRunner struct {
	calls    []fakeCall
	files    map[string]string // what a clone writes, by path
	commit   string
	version  string
	cloneErr error
	// beforeRevParse runs before rev-parse answers, to change things
	// in the middle of a sync.
	beforeRevParse func()
}

type fakeCall struct {
	dir  string
	env  []string
	args []string
}

func (f *fakeRunner) Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{dir: dir, env: env, args: args})
	switch {
	case slices.Contains(args, "clone"):
		if f.cloneErr != nil {
			return nil, f.cloneErr
		}
		target := args[len(args)-1]
		for name, body := range f.files {
			path := filepath.Join(target, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case args[0] == "rev-parse":
		if f.beforeRevParse != nil {
			f.beforeRevParse()
		}
		return []byte(f.commit + "\n"), nil
	case args[0] == "--version":
		return []byte(f.version), nil
	}
	return nil, fmt.Errorf("unexpected git call: %v", args)
}

// syncFixture is a Syncer with a fake git and a fake GitHub, and one
// resource, urtuba/demo, whose project keeps files up to 1 KB.
type syncFixture struct {
	s        *Syncer
	store    *store.Store
	runner   *fakeRunner
	github   *fakeGitHub
	project  store.Project
	resource store.Resource
	now      time.Time
}

func newSyncFixture(t *testing.T, branch, token string) *syncFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, err := st.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSyncSettings(ctx, p.ID, 6, 1024); err != nil {
		t.Fatal(err)
	}
	r, err := st.AddResource(ctx, store.NewResource{
		ProjectID: p.ID, Owner: "urtuba", Repo: "demo", Branch: branch, Token: token,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{status: http.StatusOK, size: 100}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)

	f := &syncFixture{
		store:    st,
		github:   gh,
		project:  p,
		resource: r,
		now:      testNow,
		runner: &fakeRunner{
			commit: testCommit,
			files: map[string]string{
				"README.md":   "hello",
				"src/main.go": "package main",
				"big.bin":     strings.Repeat("x", 1025),
				"edge.bin":    strings.Repeat("x", 1024),
				".git/config": "[core]",
			},
		},
	}
	s, err := New(st, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	s.runner = f.runner
	s.apiBase = srv.URL
	s.now = func() time.Time { return f.now }
	f.s = s
	return f
}

// sync runs one sync and moves the clock on, so the next sync gets a new
// folder name.
func (f *syncFixture) sync(t *testing.T) error {
	t.Helper()
	err := f.s.Sync(context.Background(), f.resource.ID)
	f.now = f.now.Add(time.Minute)
	return err
}

// load reads the fixture's resource again.
func (f *syncFixture) load(t *testing.T) store.Resource {
	t.Helper()
	r, err := f.store.Resource(context.Background(), f.project.ID, f.resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// folders lists the clone folders of the fixture's resource.
func (f *syncFixture) folders(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.s.resourceDir(f.project.ID, f.resource.ID))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// files lists every file in dir, with paths relative to it.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var list []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			list = append(list, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(list)
	return list
}

func TestSync(t *testing.T) {
	tests := []struct {
		name     string
		branch   string
		wantArgs []string // without the target folder
	}{
		{"default branch", "", []string{"-c", "core.symlinks=false", "clone", "--depth", "1", "--single-branch", "--no-tags",
			"--", "https://github.com/urtuba/demo.git"}},
		{"named branch", "release/v1.2", []string{"-c", "core.symlinks=false", "clone", "--depth", "1", "--single-branch", "--no-tags",
			"--branch=release/v1.2", "--", "https://github.com/urtuba/demo.git"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t, tt.branch, "")
			if err := f.sync(t); err != nil {
				t.Fatal(err)
			}

			if len(f.runner.calls) != 2 {
				t.Fatalf("git calls = %d, want clone and rev-parse", len(f.runner.calls))
			}
			clone := f.runner.calls[0]
			target := clone.args[len(clone.args)-1]
			if got := clone.args[:len(clone.args)-1]; !reflect.DeepEqual(got, tt.wantArgs) {
				t.Errorf("clone args = %q, want %q", got, tt.wantArgs)
			}
			name := strings.TrimPrefix(target, f.s.resourceDir(f.project.ID, f.resource.ID)+string(filepath.Separator))
			if want := fmt.Sprint(testNow.UnixNano()); name != want {
				t.Errorf("clone folder = %q, want %q in the resource folder", target, want)
			}
			if got := f.runner.calls[1]; got.dir != target || !reflect.DeepEqual(got.args, []string{"rev-parse", "HEAD"}) {
				t.Errorf("second call = %+v, want rev-parse HEAD in the clone", got)
			}

			// .git and the file over 1 KB are gone; the file of exactly 1 KB stays.
			want := []string{"README.md", "edge.bin", "src/main.go"}
			if got := files(t, target); !reflect.DeepEqual(got, want) {
				t.Errorf("files = %v, want %v", got, want)
			}

			r := f.load(t)
			if r.CloneDir != name || r.LastCommit != testCommit || !r.LastSyncedAt.Equal(testNow) || r.LastError != "" {
				t.Errorf("resource after sync = %+v", r)
			}
		})
	}
}

func TestSyncEnvironment(t *testing.T) {
	// Settings in the server's environment must not reach git, except
	// the HTTPS proxy settings.
	t.Setenv("GIT_DIR", "/tmp/elsewhere")
	t.Setenv("GIT_SSH_COMMAND", "touch /tmp/pwned")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='/tmp'")
	t.Setenv("HTTP_PROXY", "http://plain-proxy.example")
	t.Setenv("HTTPS_PROXY", "http://proxy.example")
	t.Setenv("NO_PROXY", "internal.example")
	t.Setenv("https_proxy", "") // empty values are left out
	t.Setenv("no_proxy", "")

	f := newSyncFixture(t, "", testToken)
	if err := f.sync(t); err != nil {
		t.Fatal(err)
	}
	clone, revParse := f.runner.calls[0], f.runner.calls[1]
	home := clone.dir
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_LFS_SKIP_SMUDGE=1",
		"HTTPS_PROXY=http://proxy.example",
		"NO_PROXY=internal.example",
	}
	wantClone := append(slices.Clone(base),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+basicAuth(testToken),
	)
	if !reflect.DeepEqual(clone.env, wantClone) {
		t.Errorf("clone env = %q\nwant %q", clone.env, wantClone)
	}
	if !reflect.DeepEqual(revParse.env, base) {
		t.Errorf("rev-parse env = %q\nwant %q (no token)", revParse.env, base)
	}
	if home == "" || home == os.Getenv("HOME") {
		t.Errorf("HOME = %q, want a folder of its own", home)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("git's home folder %s is still there after the sync", home)
	}

	// The token is in no argument, only in GIT_CONFIG_VALUE_0.
	for _, c := range f.runner.calls {
		for _, a := range c.args {
			if strings.Contains(a, testToken) || strings.Contains(a, basicAuth(testToken)) {
				t.Errorf("argument %q holds the token", a)
			}
		}
	}
	if want := "Bearer " + testToken; f.github.auth != want {
		t.Errorf("size check Authorization = %q, want %q", f.github.auth, want)
	}
}

func TestSizeCheck(t *testing.T) {
	tests := []struct {
		name          string
		token         string
		status        int
		size          int64
		wantErr       string // empty for success
		wantPermanent bool
	}{
		{"small public repository", "", http.StatusOK, 100, "", false},
		{"exactly 1 GB", "", http.StatusOK, 1 << 20, "", false},
		{"over 1 GB", "", http.StatusOK, 3 << 19, "repository urtuba/demo is 1.5 GB, over the 1 GB limit", true},
		{"not found", "", http.StatusNotFound, 0, "repository urtuba/demo not found, or the token has no access to it", true},
		{"bad token", testToken, http.StatusUnauthorized, 0, "the token was refused", true},
		{"rate limit", "", http.StatusForbidden, 0, "rate limit", false},
		{"GitHub is down", "", http.StatusBadGateway, 0, "size check failed with 502 Bad Gateway", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t, "", tt.token)
			f.github.status, f.github.size = tt.status, tt.size

			err := f.sync(t)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Sync() error = %v", err)
				}
				return
			}
			var se *Error
			if !errors.As(err, &se) {
				t.Fatalf("Sync() error = %v, want an *Error", err)
			}
			if !strings.Contains(se.Error(), tt.wantErr) || se.Permanent() != tt.wantPermanent {
				t.Errorf("Sync() error = %q (permanent %v), want %q (permanent %v)",
					se.Error(), se.Permanent(), tt.wantErr, tt.wantPermanent)
			}
			if len(f.runner.calls) != 0 {
				t.Errorf("git ran %d times after a failed size check", len(f.runner.calls))
			}
		})
	}

	// Without a token, the size check sends no Authorization header.
	f := newSyncFixture(t, "", "")
	if err := f.sync(t); err != nil {
		t.Fatal(err)
	}
	if f.github.auth != "" {
		t.Errorf("Authorization = %q, want none", f.github.auth)
	}
}

func TestSyncChecksNamesFirst(t *testing.T) {
	tests := []struct {
		name   string
		owner  string
		repo   string
		branch string
	}{
		{"option as owner", "-evil", "demo", ""},
		{"option as repository", "urtuba", "--upload-pack=touch", ""},
		{"path in repository", "urtuba", "../demo", ""},
		{"option as branch", "urtuba", "demo", "--upload-pack=touch"},
		{"dots in branch", "urtuba", "demo", "a..b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t, "", "")
			// The store takes any text; only the web checks names on the
			// way in. The sync must check again.
			r, err := f.store.AddResource(context.Background(), store.NewResource{
				ProjectID: f.project.ID, Owner: tt.owner, Repo: tt.repo, Branch: tt.branch,
			}, testNow)
			if err != nil {
				t.Fatal(err)
			}
			err = f.s.Sync(context.Background(), r.ID)
			var se *Error
			if !errors.As(err, &se) || !se.Permanent() {
				t.Fatalf("Sync() error = %v, want a permanent *Error", err)
			}
			if len(f.runner.calls) != 0 || f.github.calls != 0 {
				t.Errorf("git calls = %d, GitHub calls = %d, want none", len(f.runner.calls), f.github.calls)
			}
		})
	}
}

func TestCheckNames(t *testing.T) {
	tests := []struct {
		owner, repo, branch string
		ok                  bool
	}{
		{"urtuba", "repo-speaks", "", true},
		{"Urtuba-2", ".github", "feature/new_ui-2.0", true},
		{"a", "a", "a", true},
		{"", "demo", "", false},
		{"-urtuba", "demo", "", false},
		{"urt.uba", "demo", "", false},
		{"urtuba/x", "demo", "", false},
		{strings.Repeat("a", 40), "demo", "", false},
		{"urtuba", "", "", false},
		{"urtuba", ".", "", false},
		{"urtuba", "..", "", false},
		{"urtuba", "-demo", "", false},
		{"urtuba", "de mo", "", false},
		{"urtuba", "demo/x", "", false},
		{"urtuba", strings.Repeat("a", 101), "", false},
		{"urtuba", "demo", "-b", false},
		{"urtuba", "demo", "/main", false},
		{"urtuba", "demo", "main..dev", false},
		{"urtuba", "demo", "main dev", false},
		{"urtuba", "demo", "main\n", false},
		{"urtuba", "demo", "ma~in", false},
	}
	for _, tt := range tests {
		err := checkNames(tt.owner, tt.repo, tt.branch)
		if (err == nil) != tt.ok {
			t.Errorf("checkNames(%q, %q, %q) error = %v, want ok %v", tt.owner, tt.repo, tt.branch, err, tt.ok)
		}
	}
}

func TestSyncKeepsThePreviousClone(t *testing.T) {
	f := newSyncFixture(t, "", "")
	var names []string
	for range 3 {
		names = append(names, fmt.Sprint(f.now.UnixNano()))
		if len(names) == 3 {
			// Something left in the folder, such as by a crash, goes too.
			if err := os.Mkdir(filepath.Join(f.s.resourceDir(f.project.ID, f.resource.ID), "stray"), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.sync(t); err != nil {
			t.Fatal(err)
		}
		if got := f.load(t).CloneDir; got != names[len(names)-1] {
			t.Errorf("clone_dir = %q, want %q", got, names[len(names)-1])
		}
	}
	// The current and the previous clone stay, the first one is gone.
	if got, want := f.folders(t), names[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("folders = %v, want %v", got, want)
	}
}

func TestFailedSyncKeepsTheCurrentClone(t *testing.T) {
	f := newSyncFixture(t, "", "")
	if err := f.sync(t); err != nil {
		t.Fatal(err)
	}
	before := f.load(t)

	f.runner.cloneErr = errors.New("exit status 128: fatal: unable to access")
	err := f.sync(t)
	var se *Error
	if !errors.As(err, &se) || se.Permanent() {
		t.Fatalf("Sync() error = %v, want an *Error that may be tried again", err)
	}
	if !strings.Contains(se.Error(), "git clone: exit status 128") {
		t.Errorf("error = %q, want git's message", se.Error())
	}
	if got := f.load(t); got.CloneDir != before.CloneDir || got.LastCommit != before.LastCommit {
		t.Errorf("resource changed after a failed sync: %+v", got)
	}
	// The failed clone's folder is gone; the current one is untouched.
	if got, want := f.folders(t), []string{before.CloneDir}; !reflect.DeepEqual(got, want) {
		t.Errorf("folders = %v, want %v", got, want)
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	f := newSyncFixture(t, "", testToken)
	// A git that prints its whole environment and the token when it fails.
	f.runner.cloneErr = fmt.Errorf("fatal: token %s, header %s, env %s",
		testToken, basicAuth(testToken), strings.Join(f.s.env("/home", testToken), " "))
	err := f.sync(t)
	if err == nil {
		t.Fatal("Sync() error = nil, want the clone error")
	}
	for _, secret := range []string{testToken, basicAuth(testToken), "secretvalue"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q holds %q", err.Error(), secret)
		}
	}
	if !strings.Contains(err.Error(), "[hidden]") {
		t.Errorf("error = %q, want the token replaced by [hidden]", err.Error())
	}
}

func TestSyncOfARemovedResource(t *testing.T) {
	ctx := context.Background()

	t.Run("removed before", func(t *testing.T) {
		f := newSyncFixture(t, "", "")
		if err := f.store.DeleteResource(ctx, f.project.ID, f.resource.ID); err != nil {
			t.Fatal(err)
		}
		err := f.sync(t)
		var se *Error
		if !errors.As(err, &se) || !se.Permanent() {
			t.Errorf("Sync() error = %v, want a permanent *Error", err)
		}
	})

	t.Run("removed during", func(t *testing.T) {
		f := newSyncFixture(t, "", "")
		f.runner.beforeRevParse = func() {
			if err := f.store.DeleteResource(ctx, f.project.ID, f.resource.ID); err != nil {
				t.Fatal(err)
			}
		}
		err := f.sync(t)
		var se *Error
		if !errors.As(err, &se) || !se.Permanent() || !strings.Contains(se.Error(), "removed during the sync") {
			t.Errorf("Sync() error = %v, want a permanent *Error", err)
		}
		if got := f.folders(t); len(got) != 0 {
			t.Errorf("folders = %v, want the new clone deleted", got)
		}
	})
}

func TestRemoveDeleted(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t, "", "")
	gone, err := f.store.AddResource(ctx, store.NewResource{ProjectID: f.project.ID, Owner: "urtuba", Repo: "gone"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeleteResource(ctx, f.project.ID, gone.ID); err != nil {
		t.Fatal(err)
	}

	mk := func(parts ...string) string {
		dir := filepath.Join(append([]string{f.s.reposDir}, parts...)...)
		if err := os.MkdirAll(filepath.Join(dir, "1"), 0o750); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	p, r := fmt.Sprint(f.project.ID), fmt.Sprint(f.resource.ID)
	kept := mk(p, r)
	deleted := mk(p, fmt.Sprint(gone.ID))
	mk("999", r) // project 999 does not exist, though resource r does
	notOurs := mk(p, "notes")
	notAnID := mk("007")

	if err := f.s.RemoveDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		kept:                               true,
		deleted:                            false,
		filepath.Join(f.s.reposDir, "999"): false, // emptied, so removed too
		notOurs:                            true,
		notAnID:                            true,
	} {
		_, err := os.Stat(dir)
		if exists := err == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", dir, exists, want)
		}
	}
}

func TestRemoveDeletedWithoutClones(t *testing.T) {
	f := newSyncFixture(t, "", "")
	if err := f.s.RemoveDeleted(context.Background()); err != nil {
		t.Errorf("RemoveDeleted() with no repos folder: %v", err)
	}
}

func TestCheckGit(t *testing.T) {
	tests := []struct {
		version string
		wantErr string
	}{
		{"git version 2.39.3 (Apple Git-145)\n", ""},
		{"git version 2.32.0\n", ""},
		{"git version 3.0.1\n", ""},
		{"git version 2.45.1.windows.1\n", ""},
		{"git version 2.31.8\n", "needs git 2.32 or newer: found 2.31"},
		{"git version 1.99.0\n", "found 1.99"},
		{"hello\n", "cannot read the version"},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			f := newSyncFixture(t, "", "")
			f.runner.version = tt.version
			err := f.s.CheckGit(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("CheckGit() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CheckGit() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckGitWithoutGit(t *testing.T) {
	f := newSyncFixture(t, "", "")
	f.s.runner = execRunner{program: "repo-speaks-no-such-program"}
	err := f.s.CheckGit(context.Background())
	if err == nil || !strings.Contains(err.Error(), "needs git 2.32 or newer") {
		t.Errorf("CheckGit() error = %v, want a clear message", err)
	}
}

func TestClonePath(t *testing.T) {
	dir := t.TempDir()
	s, err := New(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	got := ClonePath(dir, 3, 7, "1760000000")
	if want := filepath.Join(s.resourceDir(3, 7), "1760000000"); got != want {
		t.Errorf("ClonePath() = %q, want %q, where Sync puts it", got, want)
	}
}
