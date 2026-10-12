// Package web serves Repo Speaks' server-rendered UI.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/urtuba/repo-speaks/internal/store"
)

// files holds the templates and static assets inside the binary.
//
//go:embed templates static
var files embed.FS

type server struct {
	store  *store.Store
	logger *slog.Logger
	pages  *template.Template
}

// New returns the HTTP handler for the whole UI.
func New(st *store.Store, logger *slog.Logger) http.Handler {
	s := &server{
		store:  st,
		logger: logger,
		pages:  template.Must(template.ParseFS(files, "templates/*.html")),
	}
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embedded folder always exists; a failure here is a bug
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /healthz", s.healthz)
	return middlewares(logger, mux)
}

// home renders the start page.
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	s.render(w, "home.html", nil)
}

// status renders the status fragment that htmx swaps into the page.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	data := struct{ DatabaseOK bool }{DatabaseOK: s.store.Ping(r.Context()) == nil}
	s.render(w, "status", data)
}

// healthz answers container health checks.
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		s.logger.Error("health check", "err", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("render template", "name", name, "err", err)
	}
}
