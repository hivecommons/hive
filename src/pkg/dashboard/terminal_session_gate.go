package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// User-session terminals sit on the same loopback ttyd as the agents'. ttyd is
// uncredentialed on hosted spokes, and even a credentialed one cannot keep a
// session user out: its basic-auth secret is on ttyd's command line, visible in
// /proc to any uid unless hidepid is set. So the gate is not ttyd's password
// but a per-session attach token that ONLY this process (and ttyd-tmux.sh,
// through the same entrypoint-generated env var) can compute:
//
//	token = hex(HMAC-SHA256(HIVE_TTYD_ATTACH_KEY, "hive-ttyd-attach-v1\x00" + "hive-sess-<id>"))
//
// The proxy appends it as the SECOND ttyd arg of every authorised session
// request (the browser never sees it); ttyd-tmux.sh refuses any hive-sess-*
// attach whose token does not verify, and refuses all of them when the key is
// unset. A session user who connects to 127.0.0.1:<ttyd port> directly cannot
// produce the token, so it cannot attach any session, its own included.
//
// A THIRD arg, the write token (ttydAttachWriteToken, same construction under
// its own domain string), is appended only when the caller holds the session's
// keyboard (api_sessions_keyboard.go). ttyd-tmux.sh attaches read-only
// (`attach-session -r`) unless that token verifies, so a viewer can never type,
// whatever its browser sends: the browser's request carries one arg, the
// session name, and both tokens are computed here after authorisation.
const (
	ttydAttachKeyEnv = "HIVE_TTYD_ATTACH_KEY"
	ttydAttachDomain = "hive-ttyd-attach-v1"
	// ttydWebSocketPath is ttyd's websocket endpoint under /terminal; one
	// request here is one attached terminal, for as long as it is proxied.
	ttydWebSocketPath = "/ws"
)

var sessionTmuxNamePattern = regexp.MustCompile(`^` + sessionTmuxPrefix + `[a-f0-9]{8,32}$`)

// sessionAttachModeKey carries the guard's decision (writable or not) to the
// proxy's Rewrite through the request context, never through a header.
type sessionAttachModeKey struct{}

// ttydAttachKey returns the attach key, "" when unconfigured (sessions then
// fail closed).
func ttydAttachKey() string { return strings.TrimSpace(os.Getenv(ttydAttachKeyEnv)) }

// ttydAttachToken returns the attach token for a tmux session name, "" when
// no key is configured.
func ttydAttachToken(session string) string {
	return ttydAttachTokenIn(ttydAttachDomain, session)
}

// ttydAttachTokenIn computes hex(HMAC-SHA256(key, domain + "\x00" + session)).
func ttydAttachTokenIn(domain, session string) string {
	key := ttydAttachKey()
	if key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(domain + "\x00" + session))
	return hex.EncodeToString(mac.Sum(nil))
}

// sessionAttachWritableFrom reads the guard's decision for a proxied request.
func sessionAttachWritableFrom(r *http.Request) bool {
	v, _ := r.Context().Value(sessionAttachModeKey{}).(bool)
	return v
}

// requestNamesUserSession reports whether a /terminal request asks for a user
// session (?arg=hive-sess-...). Such requests are authorised by
// guardTerminalSession, which admits the read role as a read-only viewer.
func requestNamesUserSession(r *http.Request) bool {
	for _, a := range r.URL.Query()["arg"] {
		if strings.HasPrefix(a, sessionTmuxPrefix) {
			return true
		}
	}
	return false
}

// guardTerminalSession authorises every /terminal request that names a user
// session (?arg=hive-sess-<id>): document, token endpoint and websocket alike,
// whichever credential (handoff code, assertion cookie, hub identity)
// admitted it to /terminal. Requests for anything else pass through unchanged.
// Anyone who may see the session may attach; only the keyboard holder's attach
// is writable. The websocket request is the terminal itself: it is registered
// as a live attach (presence) for as long as the proxy keeps it open.
func (s *Server) guardTerminalSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := r.URL.Query()["arg"]
		if !requestNamesUserSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Exactly one arg, and it must be a well-formed session name: extra
		// args would reach ttyd-tmux.sh's argv.
		if len(args) != 1 || !sessionTmuxNamePattern.MatchString(args[0]) {
			http.Error(w, "invalid session terminal request", http.StatusBadRequest)
			return
		}
		if _, ok := s.sessionsRuntime(); !ok {
			http.Error(w, "sessions are not enabled on this hive", http.StatusNotFound)
			return
		}
		rec, found := s.sessionStoreOrNil().get(strings.TrimPrefix(args[0], sessionTmuxPrefix))
		actor := s.sessionActorOf(r)
		if !found || !canViewSession(actor, rec) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if rec.Status != sessionStatusRunning {
			http.Error(w, "session is not running", http.StatusConflict)
			return
		}
		if ttydAttachKey() == "" {
			http.Error(w, "session terminals are unavailable: the ttyd attach key is not configured", http.StatusServiceUnavailable)
			return
		}
		writable := sessionAttachWritable(actor, rec)
		r = r.WithContext(context.WithValue(r.Context(), sessionAttachModeKey{}, writable))
		if strings.TrimPrefix(r.URL.Path, terminalPathPrefix) == ttydWebSocketPath {
			attachID := s.beginSessionAttach(rec.ID, actor.user, writable)
			defer s.endSessionAttach(attachID)
		}
		next.ServeHTTP(w, r)
	})
}
