package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urtuba/repo-speaks/internal/agent"
	"github.com/urtuba/repo-speaks/internal/auth"
	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
)

// testPassword is the password of every account made by testApp.
const testPassword = "correct horse battery"

// fakeClock is a clock that tests move by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// testApp is the whole UI on a fresh database, with a fake clock. Like
// the real server, it refuses LLM hosts on private addresses. Answers
// run in the background, as on the real server; answers.Wait waits for
// them.
type testApp struct {
	t       *testing.T
	h       http.Handler
	store   *store.Store
	clock   *fakeClock
	logs    *syncBuffer // everything the app logged
	dataDir string      // the database and the clones
	answers *agent.Service
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := &fakeClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	a := &testApp{t: t, store: st, clock: clock, logs: &syncBuffer{}, dataDir: dataDir}
	a.useLLM(llm.New(false))
	return a
}

// useLLM rebuilds the app with another LLM client. Tests that call a
// fake host on 127.0.0.1 need one that allows private addresses. The
// login throttle and the rate limit counts start over; sessions stay,
// since they are rows.
func (a *testApp) useLLM(c *llm.Client) {
	logger := slog.New(slog.NewTextHandler(a.logs, nil))
	answers := agent.New(a.store, c, a.dataDir, logger)
	ctx, cancel := context.WithCancel(context.Background())
	a.t.Cleanup(func() {
		cancel()
		answers.Wait()
	})
	if err := answers.Start(ctx); err != nil {
		a.t.Fatal(err)
	}
	a.answers = answers
	a.h = newHandler(a.store, logger, c, answers, a.clock.now)
}

// syncBuffer is a bytes.Buffer that answers may write to from their own
// goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// get sends a GET request, with the session cookie when c is not nil.
func (a *testApp) get(path string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

// post sends a form like a browser on the same site, so it passes the
// cross-origin check.
func (a *testApp) post(path string, form url.Values, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

// createUser makes an account with testPassword. The first owner goes
// through CreateOwner, like setup does.
func (a *testApp) createUser(username string, owner bool) store.User {
	a.t.Helper()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		a.t.Fatal(err)
	}
	var u store.User
	if owner {
		u, err = a.store.CreateOwner(context.Background(), username, hash, a.clock.now())
	} else {
		u, err = a.store.CreateUser(context.Background(), username, hash, a.clock.now())
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

// login logs in and returns the session cookie.
func (a *testApp) login(username string) *http.Cookie {
	a.t.Helper()
	rec := a.post("/login", url.Values{"username": {username}, "password": {testPassword}}, nil)
	if rec.Code != http.StatusSeeOther {
		a.t.Fatalf("login as %s: status = %d, want %d\n%s", username, rec.Code, http.StatusSeeOther, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil {
		a.t.Fatalf("login as %s: no session cookie", username)
	}
	return c
}

// sessionCookie returns the session cookie the response sets, or nil.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}
