package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/store"
)

// newDataDir returns a data folder whose database holds the owner samed.
func newDataDir(t *testing.T) (dir string, getenv func(string) string) {
	t.Helper()
	dir = t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "repo-speaks.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateOwner(context.Background(), "samed", "hash", time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return dir, func(key string) string {
		if key == "REPO_SPEAKS_DATA_DIR" {
			return dir
		}
		return ""
	}
}

// printedToken returns the token from the path that resetLink printed.
func printedToken(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if token, ok := strings.CutPrefix(line, "/reset/"); ok {
			return token
		}
	}
	t.Fatalf("no reset path in output:\n%s", out)
	return ""
}

func TestResetLinkForOwner(t *testing.T) {
	ctx := context.Background()
	dir, getenv := newDataDir(t)
	now := time.Now()

	var first, second bytes.Buffer
	if err := resetLink(ctx, getenv, "samed", &first, now); err != nil {
		t.Fatal(err)
	}
	if err := resetLink(ctx, getenv, "samed", &second, now); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dir, "repo-speaks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	reset, err := st.PasswordResetByHash(ctx, auth.HashToken(printedToken(t, second.String())))
	if err != nil {
		t.Fatal(err)
	}
	if reset.Username != "samed" || !reset.Open(now) {
		t.Errorf("newest link: got user %q, open %v; want samed, open", reset.Username, reset.Open(now))
	}
	if reset.Open(now.Add(auth.ResetLifetime)) {
		t.Error("link still open after 24 hours")
	}
	// The newest link wins, as with links the owner makes on the web.
	if _, err := st.PasswordResetByHash(ctx, auth.HashToken(printedToken(t, first.String()))); err == nil {
		t.Error("older link still exists")
	}
}

func TestResetLinkUnknownAccount(t *testing.T) {
	_, getenv := newDataDir(t)
	err := resetLink(context.Background(), getenv, "nobody", new(bytes.Buffer), time.Now())
	if err == nil || !strings.Contains(err.Error(), `no account named "nobody"`) {
		t.Errorf("got %v, want no account named \"nobody\"", err)
	}
}

func TestResetLinkMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	getenv := func(key string) string {
		if key == "REPO_SPEAKS_DATA_DIR" {
			return dir
		}
		return ""
	}
	err := resetLink(context.Background(), getenv, "samed", new(bytes.Buffer), time.Now())
	if err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("got %v, want no database", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*")); len(matches) > 0 {
		t.Errorf("command created files: %v", matches)
	}
}
