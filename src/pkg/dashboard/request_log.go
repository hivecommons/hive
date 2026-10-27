package dashboard

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/logread"
)

// The dashboard request log (hivecommons/hive#11944) is the log source
// "requests": one slog JSON record per request that reaches this server, with
// its time, method, path with query values removed, status, duration and
// authentication outcome. Terminal traffic the Node auth proxy handles itself
// and requests the gateway refuses never reach this server, so they are not in
// it.
const (
	// authOutcomePublic: the path needs no authentication.
	authOutcomePublic = "public"
	// authOutcomeOK: the request was authenticated (or the dashboard is open
	// by design, with no token and no allowlist).
	authOutcomeOK = "ok"
	// authOutcomeFailed: credentials were presented and refused.
	authOutcomeFailed = "failed"
	// authOutcomeNone: no credentials on a path that needs them.
	authOutcomeNone = "none"

	// requestLogPathMax bounds the recorded path so one long URL cannot fill
	// a file.
	requestLogPathMax = 2048
)

// outputLogDir is where the request log is written and the output sources are
// read; TestMain points it at a per-run directory.
var outputLogDir = func() string { return logread.OutputDir(os.Getenv) }

// authProxyEntryPath is the Node auth proxy the spoke image ships and the
// container start-up script runs; the hub image has none.
var authProxyEntryPath = "/opt/hive/proxy/server.js"

func authProxyInstalled() bool {
	st, err := os.Stat(authProxyEntryPath)
	return err == nil && st.Mode().IsRegular()
}

// openRequestLog turns the request log on for a serving dashboard. A failure
// is logged and leaves requests unrecorded; it never stops the dashboard.
func (s *Server) openRequestLog() {
	if s.requestLog != nil {
		return
	}
	out, err := logread.OpenOutputLog(outputLogDir(), logread.SourceRequests)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("dashboard request log unavailable; requests are not recorded", "error", err)
		}
		return
	}
	s.requestLog = slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// authenticatedMux is the authenticated part of both handler chains, Start()
// and Handler(). markAuthenticated sits just inside authenticate, so it runs
// only for a request authenticate let through.
func (s *Server) authenticatedMux() http.Handler {
	return s.authenticate(s.markAuthenticated(s.roleEnforcement(s.securityHeaders(s.mux))))
}

type requestLogStateKey struct{}

// requestLogState carries what the request log learns while a request is
// served.
type requestLogState struct {
	admitted bool
}

// markAuthenticated records that authenticate admitted the request.
func (s *Server) markAuthenticated(next http.Handler) http.Handler {
	if s.requestLog == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if st, ok := r.Context().Value(requestLogStateKey{}).(*requestLogState); ok {
			st.admitted = true
		}
		next.ServeHTTP(w, r)
	})
}

// withRequestLog records each request once it has been served. With no
// request log open it returns next unchanged.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	if s.requestLog == nil {
		return next
	}
	logger := s.requestLog
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		public := s.isPublicPath(r.URL.Path)
		presented := presentsCredentials(r)
		method, path := r.Method, requestLogPath(r.URL)
		state := &requestLogState{}
		rw := &requestLogResponseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), requestLogStateKey{}, state)))
		status := rw.finalStatus()
		logger.LogAttrs(context.Background(), requestLogLevel(status), "request",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("auth", authOutcome(public, state.admitted, presented)),
		)
	})
}

func authOutcome(public, admitted, presented bool) string {
	switch {
	case public:
		return authOutcomePublic
	case admitted:
		return authOutcomeOK
	case presented:
		return authOutcomeFailed
	default:
		return authOutcomeNone
	}
}

func requestLogLevel(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// presentsCredentials reports whether a request carries anything authenticate
// could accept as a credential. It is read before authenticate strips the
// inbound identity headers.
func presentsCredentials(r *http.Request) bool {
	for _, h := range []string{"Authorization", "X-Hive-Internal", proxyAuthHeader, "X-Hive-User", "X-Hive-Role"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	q := r.URL.Query()
	if q.Get("token") != "" || q.Get(terminalHandoffCodeParam) != "" {
		return true
	}
	for _, name := range []string{sessionCookieName, terminalAssertionCookieName} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return true
		}
	}
	return false
}

// requestLogPath is the request's path with every query value removed; the
// query keys stay, so "?token=abc&lines=5" is recorded as "?token=&lines=".
func requestLogPath(u *url.URL) string {
	p := u.EscapedPath()
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, part := range parts {
			key, _, _ := strings.Cut(part, "=")
			parts[i] = key + "="
		}
		p += "?" + strings.Join(parts, "&")
	}
	if len(p) > requestLogPathMax {
		p = p[:requestLogPathMax]
	}
	return p
}

// requestLogResponseWriter notes the status a handler sends. Like
// serverTimingResponseWriter, it passes flushing, hijacking (websocket
// upgrades), server push and ReadFrom straight through, and Unwrap lets
// http.ResponseController reach the connection.
type requestLogResponseWriter struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *requestLogResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *requestLogResponseWriter) noteStatus(status int) {
	if w.status == 0 && (status >= http.StatusOK || status == http.StatusSwitchingProtocols) {
		w.status = status
	}
}

// finalStatus is the status the client saw: a hijacked connection that sent
// none of its own through WriteHeader is a protocol switch, and a handler that
// wrote nothing got net/http's implicit 200.
func (w *requestLogResponseWriter) finalStatus() int {
	switch {
	case w.status != 0:
		return w.status
	case w.hijacked:
		return http.StatusSwitchingProtocols
	default:
		return http.StatusOK
	}
}

func (w *requestLogResponseWriter) WriteHeader(status int) {
	w.noteStatus(status)
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestLogResponseWriter) Write(b []byte) (int, error) {
	w.noteStatus(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

func (w *requestLogResponseWriter) Flush() {
	w.noteStatus(http.StatusOK)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *requestLogResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	w.hijacked = true
	return h.Hijack()
}

func (w *requestLogResponseWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (w *requestLogResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	w.noteStatus(http.StatusOK)
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}
