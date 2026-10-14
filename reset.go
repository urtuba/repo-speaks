package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/config"
	"github.com/urtuba/repo-speaks/internal/store"
	"github.com/urtuba/repo-speaks/internal/web"
)

// resetLink makes a password reset link for an account and prints its
// path. It is the way back in for an owner who forgot their password,
// since nobody else can make a link for the owner. It works while the
// server runs: SQLite lets the two processes share the database.
func resetLink(ctx context.Context, getenv func(string) string, username string, stdout io.Writer, now time.Time) error {
	cfg := config.Load(getenv)
	path := filepath.Join(cfg.DataDir, "repo-speaks.db")
	// Opening a missing file would create an empty database, so check first.
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no database at %s: set REPO_SPEAKS_DATA_DIR to the server's data folder", path)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer st.Close()

	user, err := st.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account named %q", username)
	}
	if err != nil {
		return err
	}

	token, hash := auth.NewToken()
	expires := auth.ResetExpiry(now)
	// No maker: the link comes from the server, not from an account.
	if err := st.CreatePasswordReset(ctx, user.ID, hash, 0, now, expires); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Reset link for %s. It works once, until %s.\n", user.Username, expires.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintln(stdout, "Put your server's address in front of this path:")
	fmt.Fprintln(stdout, web.ResetPath(token))
	return nil
}
