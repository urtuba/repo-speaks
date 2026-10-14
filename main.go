// Command repo-speaks runs the Repo Speaks server: the web UI, the
// background jobs that sync repositories, and the agents, all in one
// process.
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

	"github.com/urtuba/repo-speaks/internal/agent"
	"github.com/urtuba/repo-speaks/internal/config"
	"github.com/urtuba/repo-speaks/internal/jobs"
	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
	"github.com/urtuba/repo-speaks/internal/sync"
	"github.com/urtuba/repo-speaks/internal/web"
)

// usage lists the commands. With no command, the server starts.
const usage = `usage:
  repo-speaks                        start the server
  repo-speaks reset-link <username>  print a password reset link`

func main() {
	ctx := context.Background()
	var err error
	switch args := os.Args[1:]; {
	case len(args) == 0:
		err = run(ctx, os.Getenv, os.Stderr)
	case len(args) == 2 && args[0] == "reset-link":
		err = resetLink(ctx, os.Getenv, args[1], os.Stdout, time.Now())
	default:
		err = errors.New(usage)
	}
	if err != nil {
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
	if err := cfg.Check(); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data folder: %w", err)
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "repo-speaks.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	syncer, err := sync.New(st, cfg.DataDir)
	if err != nil {
		return err
	}
	// Without git no repository can sync, so stop now with a clear message.
	if err := syncer.CheckGit(ctx); err != nil {
		return err
	}

	if cfg.AllowPrivateLLM {
		logger.Warn("models may use hosts on private addresses (REPO_SPEAKS_ALLOW_PRIVATE_LLM)")
	}
	llmClient := llm.New(cfg.AllowPrivateLLM)

	g, ctx := errgroup.WithContext(ctx)

	// Answers run in the background on ctx, so they stop with the server.
	// Start marks the answers an earlier run left running as failed.
	// Wait runs before the database closes, so each stopped answer can
	// record why.
	answers := agent.New(st, llmClient, cfg.DataDir, logger)
	if err := answers.Start(ctx); err != nil {
		return err
	}
	defer answers.Wait()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           web.New(st, logger, llmClient, answers),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Answers run in the background and pages poll for them, so no
		// handler writes for long. The slowest one checks a model with
		// its host, which takes 30 seconds at most.
		WriteTimeout: time.Minute,
	}
	g.Go(func() error {
		logger.Info("listening", "addr", cfg.Addr, "data", cfg.DataDir)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("web server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done() // Ctrl+C, SIGTERM, or another goroutine failed
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	// The jobs loop stops when ctx ends: a running sync is cut off and
	// goes back to the queue for the next start.
	g.Go(func() error {
		return jobs.New(st, syncer, logger).Run(ctx)
	})
	return g.Wait()
}
