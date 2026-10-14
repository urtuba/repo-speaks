package web

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	h := middlewares(slog.New(slog.NewTextHandler(io.Discard, nil)), http.NotFoundHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"Content-Security-Policy": contentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}
	for name, value := range want {
		if got := rec.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want none (the proxy sets it)", got)
	}
}

func TestCrossOriginProtection(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := middlewares(slog.New(slog.NewTextHandler(io.Discard, nil)), ok)

	tests := []struct {
		name       string
		method     string
		header     string
		value      string
		wantStatus int
	}{
		{"same-origin post", http.MethodPost, "Sec-Fetch-Site", "same-origin", http.StatusOK},
		{"cross-site post", http.MethodPost, "Sec-Fetch-Site", "cross-site", http.StatusForbidden},
		{"cross-site get", http.MethodGet, "Sec-Fetch-Site", "cross-site", http.StatusOK},
		{"old browser, other origin", http.MethodPost, "Origin", "https://evil.example", http.StatusForbidden},
		{"non-browser post", http.MethodPost, "", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://repo-speaks.example/", nil)
			if tt.header != "" {
				req.Header.Set(tt.header, tt.value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestLimitBody(t *testing.T) {
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			t.Logf("limit = %d", maxErr.Limit)
		}
	})
	h := middlewares(slog.New(slog.NewTextHandler(io.Discard, nil)), read)

	tests := []struct {
		size       int
		wantStatus int
	}{
		{maxBodyBytes, http.StatusOK},
		{maxBodyBytes + 1, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(make([]byte, tt.size)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.wantStatus {
			t.Errorf("body of %d bytes: status = %d, want %d", tt.size, rec.Code, tt.wantStatus)
		}
	}
}

func TestRecoverPanics(t *testing.T) {
	var logs bytes.Buffer
	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	h := middlewares(slog.New(slog.NewTextHandler(&logs, nil)), boom)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(logs.String(), "err=boom") {
		t.Errorf("logs do not mention the panic:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "status=500") {
		t.Errorf("request log does not show status 500:\n%s", logs.String())
	}
}

func TestRecoverPanicsKeepsAbort(t *testing.T) {
	abort := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
	h := recoverPanics(slog.New(slog.NewTextHandler(io.Discard, nil)), abort)

	defer func() {
		if err := recover(); err != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", err)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// TestLogsHideTokens checks that no log line holds a link's token, for a
// matched route, a wrong method, a path no route matches, and a panic.
func TestLogsHideTokens(t *testing.T) {
	const token = "s3cret-token"
	var logs bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /reset/{token}", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /invites/{token}", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	h := middlewares(slog.New(slog.NewTextHandler(&logs, nil)), noteRoute(mux))

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/reset/"+token, nil),
		httptest.NewRequest(http.MethodPut, "/reset/"+token, nil),
		httptest.NewRequest(http.MethodGet, "/reset/"+token+"/extra", nil),
		httptest.NewRequest(http.MethodGet, "/invites/"+token, nil),
	} {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	if strings.Contains(logs.String(), token) {
		t.Errorf("logs hold the token:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `route="GET /reset/{token}"`) {
		t.Errorf("logs do not name the matched route:\n%s", logs.String())
	}
}

// TestLogsNameRouteBehindSession checks the full handler: the session
// middleware passes the mux a copy of the request, and the log must
// still name the route a logged-in request matched.
func TestLogsNameRouteBehindSession(t *testing.T) {
	a := newTestApp(t)
	a.createUser("samed", true)
	c := a.login("samed")
	a.get("/projects/123", c)
	if !strings.Contains(a.logs.String(), `route="GET /projects/{id}"`) {
		t.Errorf("log does not name the route:\n%s", a.logs.String())
	}
}
