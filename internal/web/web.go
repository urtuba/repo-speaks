// Package web serves Repo Speaks' server-rendered UI.
package web

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
)

// files holds the templates and static assets inside the binary.
//
//go:embed templates static
var files embed.FS

type server struct {
	store    *store.Store
	logger   *slog.Logger
	llm      *llm.Client // checks and tests models on their hosts
	pages    *template.Template
	throttle *auth.Throttle
	now      func() time.Time
}

// New returns the HTTP handler for the whole UI. llmClient checks a
// model's ID when it is saved, and runs the Test button.
func New(st *store.Store, logger *slog.Logger, llmClient *llm.Client) http.Handler {
	return newHandler(st, logger, llmClient, time.Now)
}

// newHandler is New with a clock, so tests can move time forward.
func newHandler(st *store.Store, logger *slog.Logger, llmClient *llm.Client, now func() time.Time) http.Handler {
	s := &server{
		store:    st,
		logger:   logger,
		llm:      llmClient,
		pages:    template.Must(template.ParseFS(files, "templates/*.html")),
		throttle: auth.NewThrottle(now),
		now:      now,
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
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setup)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("POST /projects", requireLogin(s.createProject))
	mux.HandleFunc("GET /projects/{id}", requireLogin(s.project))
	mux.HandleFunc("POST /projects/{id}/invites", requireLogin(s.createInvite))
	mux.HandleFunc("POST /projects/{id}/invites/{invite}/revoke", requireLogin(s.revokeInvite))
	mux.HandleFunc("GET /projects/{id}/members/{user}", requireLogin(s.memberPage))
	mux.HandleFunc("POST /projects/{id}/members/{user}", requireLogin(s.saveMember))
	mux.HandleFunc("POST /projects/{id}/members/{user}/remove", requireLogin(s.removeMember))
	mux.HandleFunc("POST /projects/{id}/leave", requireLogin(s.leaveProject))
	mux.HandleFunc("GET /projects/{id}/resources", requireLogin(s.resourcesFragment))
	mux.HandleFunc("POST /projects/{id}/resources", requireLogin(s.addResource))
	mux.HandleFunc("GET /projects/{id}/resources/{resource}", requireLogin(s.resourcePageView))
	mux.HandleFunc("POST /projects/{id}/resources/{resource}/sync", requireLogin(s.syncResource))
	mux.HandleFunc("POST /projects/{id}/resources/{resource}/token", requireLogin(s.setResourceToken))
	mux.HandleFunc("POST /projects/{id}/resources/{resource}/token/clear", requireLogin(s.clearResourceToken))
	mux.HandleFunc("POST /projects/{id}/resources/{resource}/remove", requireLogin(s.removeResource))
	mux.HandleFunc("GET /projects/{id}/settings", requireLogin(s.settings))
	mux.HandleFunc("POST /projects/{id}/settings", requireLogin(s.saveSettings))
	mux.HandleFunc("POST /projects/{id}/settings/sync", requireLogin(s.saveSyncSettings))
	mux.HandleFunc("POST /projects/{id}/settings/default-model", requireLogin(s.setDefaultModel))
	mux.HandleFunc("POST /projects/{id}/models", requireLogin(s.addModel))
	mux.HandleFunc("GET /projects/{id}/models/{model}", requireLogin(s.modelPageView))
	mux.HandleFunc("POST /projects/{id}/models/{model}", requireLogin(s.saveModel))
	mux.HandleFunc("POST /projects/{id}/models/{model}/test", requireLogin(s.testModel))
	mux.HandleFunc("POST /projects/{id}/models/{model}/delete", requireLogin(s.deleteModel))
	mux.HandleFunc("POST /projects/{id}/delete", requireLogin(s.deleteProject))
	mux.HandleFunc("GET /account", requireLogin(s.account))
	mux.HandleFunc("POST /account", requireLogin(s.changePassword))
	mux.HandleFunc("GET /accounts", requireLogin(s.accounts))
	mux.HandleFunc("POST /accounts/{user}/reset", requireLogin(s.createReset))
	mux.HandleFunc("GET /reset/{token}", s.showReset)
	mux.HandleFunc("POST /reset/{token}", s.useReset)
	mux.HandleFunc("GET /invites/{token}", s.showInvite)
	mux.HandleFunc("POST /invites/{token}", s.acceptInvite)
	return middlewares(logger, s.loadSession(noteRoute(mux)))
}

// status renders the status fragment that htmx swaps into the page.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	data := struct{ DatabaseOK bool }{DatabaseOK: s.store.Ping(r.Context()) == nil}
	s.render(w, http.StatusOK, "status", data)
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

// render writes the named template with the status code. It renders into
// a buffer first, so a template error gives a clean 500 and not half a page.
func (s *server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.logger.Error("render template", "name", name, "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// serverError logs err and answers 500 without details.
func (s *server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request failed", "method", r.Method, "route", logRoute(r), "err", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// parseForm reads the form body. On failure it answers and returns false:
// 413 for a body over the limit, 400 for anything else.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	err := r.ParseForm()
	if err == nil {
		return true
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return false
	}
	http.Error(w, "bad form", http.StatusBadRequest)
	return false
}
