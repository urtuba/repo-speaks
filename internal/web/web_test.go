package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urtuba/repo-speaks/internal/agent"
	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
)

func TestRoutes(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(st, logger, llm.New(false), agent.New(st, llm.New(false), t.TempDir(), logger))

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/", http.StatusSeeOther, ""}, // not logged in
		{"/setup", http.StatusOK, `<link rel="stylesheet" href="/static/app.css">`},
		{"/static/app.css", http.StatusOK, "--bg"},
		{"/static/app.js", http.StatusOK, "data-copy"},
		{"/setup", http.StatusOK, `<link rel="icon" href="/static/logo.svg" type="image/svg+xml">`},
		{"/static/logo.svg", http.StatusOK, "<svg"},
		{"/status", http.StatusOK, "Database: ok"},
		{"/healthz", http.StatusOK, "ok"},
		{"/static/htmx.min.js", http.StatusOK, "htmx"},
		{"/missing", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("GET %s status = %d, want %d", tt.path, rec.Code, tt.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("GET %s body does not contain %q", tt.path, tt.wantBody)
			}
		})
	}
}
