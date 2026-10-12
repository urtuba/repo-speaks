package web

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// maxBodyBytes caps every request body.
const maxBodyBytes = 1 << 20 // 1 MB

// contentSecurityPolicy lets pages load only our own files. default-src
// does not cover base-uri, form-action and frame-ancestors, so they are
// listed too. htmx must run with allowEval off, or hx-on would break.
const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// middlewares wraps the handler with every middleware, outermost first.
// logRequests comes first so it also logs the 500 of a recovered panic.
func middlewares(logger *slog.Logger, next http.Handler) http.Handler {
	h := limitBody(maxBodyBytes, next)
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(h)
	h = recoverPanics(logger, h)
	return logRequests(logger, h)
}

// logRequests logs every request with its status and duration.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}

// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the original writer,
// which streaming (Server-Sent Events) will need.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// recoverPanics answers 500 when a handler panics, and logs the stack.
// Without it, net/http only closes the connection.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			err := recover()
			if err == nil {
				return
			}
			if err == http.ErrAbortHandler {
				panic(err) // a deliberate abort; net/http handles it quietly
			}
			logger.Error("panic", "path", r.URL.Path, "err", err, "stack", string(debug.Stack()))
			// If the handler already wrote a status, this one is ignored.
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets headers that make the browser stricter.
// HSTS is left to the reverse proxy: the app cannot know whether HTTPS is used.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// limitBody makes reading more than n bytes of a body fail with
// *http.MaxBytesError. The handler decides how to answer.
func limitBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next.ServeHTTP(w, r)
	})
}
