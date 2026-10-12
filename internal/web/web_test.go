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

	"github.com/urtuba/repo-speaks/internal/store"
)

func TestRoutes(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handler := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/", http.StatusSeeOther, ""}, // not logged in
		{"/setup", http.StatusOK, `<link rel="stylesheet" href="/static/app.css">`},
		{"/static/app.css", http.StatusOK, "--bg"},
		{"/static/app.js", http.StatusOK, "data-copy"},
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
