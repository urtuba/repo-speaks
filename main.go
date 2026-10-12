// Command repo-speaks runs the Repo Speaks server: the web UI, and later the
// agents and sync workers, all in one process.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/urtuba/repo-speaks/internal/config"
	"github.com/urtuba/repo-speaks/internal/store"
	"github.com/urtuba/repo-speaks/internal/web"
)

func main() {
	if err := run(context.Background(), os.Getenv, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run starts everything and blocks until Ctrl+C, SIGTERM or a failure.
// Its inputs are parameters so a test can call it without a real environment.
func run(ctx context.Context, getenv func(string) string, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.Load(getenv)
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data folder: %w", err)
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "repo-speaks.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           web.New(st, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Info("listening", "addr", cfg.Addr, "data", cfg.DataDir)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("web server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done() // Ctrl+C, SIGTERM, or the server goroutine failed
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}
