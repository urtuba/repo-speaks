package sync

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	gosync "sync"
	"testing"

	"github.com/urtuba/repo-speaks/internal/store"
)

// TestSyncWithRealGit clones for real: a local bare repository, served
// over HTTPS by git http-backend, the way GitHub serves one.
func TestSyncWithRealGit(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	work := t.TempDir()

	// git runs a git command for the test's own setup.
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + work,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	// The repository: a README, a file over the limit, and a symlink
	// that points out of the clone.
	src := filepath.Join(work, "src")
	git(work, "init", "-q", "-b", "main", src)
	write(filepath.Join(src, "README.md"), "hello\n")
	write(filepath.Join(src, "docs", "big.txt"), strings.Repeat("x", 2000))
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	git(src, "add", "-A")
	git(src, "commit", "-q", "-m", "first")
	served := filepath.Join(work, "served")
	bare := filepath.Join(served, "urtuba", "demo.git")
	git(work, "clone", "-q", "--bare", src, bare)

	// One HTTPS server plays both GitHub's API and its git host.
	var mu gosync.Mutex
	var apiAuth, gitAuth string
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "HOME=" + work,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/urtuba/demo", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		apiAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte(`{"size": 12}`))
	})
	mux.HandleFunc("/urtuba/demo.git/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gitAuth = r.Header.Get("Authorization")
		mu.Unlock()
		backend.ServeHTTP(w, r)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	caFile := filepath.Join(work, "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o640); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := st.CreateProject(ctx, "Customer portal", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSyncSettings(ctx, p.ID, 6, 1000); err != nil {
		t.Fatal(err)
	}
	res, err := st.AddResource(ctx, store.NewResource{ProjectID: p.ID, Owner: "urtuba", Repo: "demo", Token: testToken}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	s.apiBase, s.gitBase, s.caFile = srv.URL, srv.URL, caFile
	s.client = srv.Client()

	// syncOnce syncs and returns the new clone's folder.
	syncOnce := func() string {
		t.Helper()
		if err := s.Sync(ctx, res.ID); err != nil {
			t.Fatal(err)
		}
		r, err := st.Resource(ctx, p.ID, res.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := git(src, "rev-parse", "HEAD"); r.LastCommit != want {
			t.Errorf("last commit = %q, want %q", r.LastCommit, want)
		}
		return r.CloneDir
	}

	first := syncOnce()
	clone := filepath.Join(s.resourceDir(p.ID, res.ID), first)
	if got := files(t, clone); !reflect.DeepEqual(got, []string{"README.md", "link"}) {
		t.Errorf("files = %v, want README.md and link (no .git, no big file)", got)
	}
	info, err := os.Lstat(filepath.Join(clone, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("link has mode %v, want a plain file", info.Mode())
	}
	mu.Lock()
	if want := "Bearer " + testToken; apiAuth != want {
		t.Errorf("API Authorization = %q, want %q", apiAuth, want)
	}
	if want := "Basic " + basicAuth(testToken); gitAuth != want {
		t.Errorf("git Authorization = %q, want %q", gitAuth, want)
	}
	mu.Unlock()

	// Two more commits and syncs: the current and the previous clone stay.
	var names []string
	for _, body := range []string{"second\n", "third\n"} {
		write(filepath.Join(src, "README.md"), body)
		git(src, "commit", "-q", "-am", "next")
		git(src, "push", "-q", bare, "main")
		names = append(names, syncOnce())
	}
	got, err := os.ReadDir(s.resourceDir(p.ID, res.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name() != names[0] || got[1].Name() != names[1] {
		t.Errorf("folders = %v, want %v", got, names)
	}
	body, err := os.ReadFile(filepath.Join(s.resourceDir(p.ID, res.ID), names[1], "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "third\n" {
		t.Errorf("README.md = %q, want the latest commit", body)
	}
}
