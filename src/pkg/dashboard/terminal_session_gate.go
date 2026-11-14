package dashboard

import (
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
const (
	ttydAttachKeyEnv = "HIVE_TTYD_ATTACH_KEY"
	ttydAttachDomain = "hive-ttyd-attach-v1"
)

var sessionTmuxNamePattern = regexp.MustCompile(`^` + sessionTmuxPrefix + `[a-f0-9]{8,32}$`)

// ttydAttachKey returns the attach key, "" when unconfigured (sessions then
// fail closed).
func ttydAttachKey() string { return strings.TrimSpace(os.Getenv(ttydAttachKeyEnv)) }

// ttydAttachToken returns the attach token for a tmux session name, "" when
// no key is configured.
func ttydAttachToken(session string) string {
	key := ttydAttachKey()
	if key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(ttydAttachDomain + "\x00" + session))
	return hex.EncodeToString(mac.Sum(nil))
}

// guardTerminalSession authorises every /terminal request that names a user
// session (?arg=hive-sess-<id>): document, token endpoint and websocket alike,
// whichever credential (handoff code, assertion cookie, hub identity)
// admitted it to /terminal. Requests for anything else pass through unchanged.
func (s *Server) guardTerminalSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := r.URL.Query()["arg"]
		named := false
		for _, a := range args {
			if strings.HasPrefix(a, sessionTmuxPrefix) {
				named = true
			}
		}
		if !named {
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
		if !canUseSession(actor, rec) {
			http.Error(w, "only the session's creator or an owner may attach a private session", http.StatusForbidden)
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
		next.ServeHTTP(w, r)
	})
}
