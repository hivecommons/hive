package dashboard

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"time"

	hub "github.com/hivecommons/hive/pkg/hub/spoke"
)

// userSession is a per-user server-side session created after a successful,
// AUTHORIZED device-flow login on a direct-route spoke. Sessions are keyed by a
// random opaque id held in the client's hive_session cookie, so two different
// GitHub users get two distinct sessions and each request resolves to the user
// that owns ITS cookie — never a single shared identity.
//
// The GitHub OAuth token is intentionally NOT stored on the session; the token
// lives only in the persisted userTokenPath file and is never exposed through
// the session. The session carries only identity + role.
type userSession struct {
	Username  string
	Role      string
	ExpiresAt time.Time
}

// sessionIDBytes is the entropy of an opaque session id. 32 bytes (256 bits) is
// well beyond guessing range and matches the dashboard auth token strength.
const sessionIDBytes = 32

// sessionTTL bounds how long a device-flow session remains valid before the
// user must re-authenticate. It mirrors the cookie MaxAge.
const sessionTTL = time.Duration(sessionCookieMaxAge) * time.Second

// EnableSessionPersistence makes per-user sessions survive process restarts
// by mirroring the session map to path (created 0600 — session ids are bearer
// credentials, so the file lives in the same trust domain as dashboard-token).
// Without this, direct-route spokes log every user out on every pod restart:
// the hive_session cookie lasts 30 days but the map it resolves against was
// memory-only, and a direct-route spoke has no hub gateway to re-inject
// identity headers. Call once at startup before the server begins serving.
func (s *Server) EnableSessionPersistence(path string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.sessionStorePath = path

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) && s.logger != nil {
			s.logger.Warn("session store unreadable — starting with no restored sessions", "path", path, "error", err)
		}
		return
	}
	var stored map[string]*userSession
	if err := json.Unmarshal(data, &stored); err != nil {
		if s.logger != nil {
			s.logger.Warn("session store corrupt — starting with no restored sessions", "path", path, "error", err)
		}
		return
	}
	now := time.Now()
	restored := 0
	for id, sess := range stored {
		if now.After(sess.ExpiresAt) {
			continue
		}
		s.userSessions[id] = sess
		restored++
	}
	if restored > 0 && s.logger != nil {
		s.logger.Info("restored dashboard sessions", "count", restored, "path", path)
	}
}

// persistSessionsLocked mirrors the session map to the store path, if
// persistence is enabled. Callers must hold sessionMu for writing. The file is
// small (one entry per live login), so a full atomic rewrite per mutation is
// cheaper than the complexity of anything incremental.
func (s *Server) persistSessionsLocked() {
	if s.sessionStorePath == "" {
		return
	}
	data, err := json.Marshal(s.userSessions)
	if err != nil {
		return
	}
	tmp := s.sessionStorePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		if s.logger != nil {
			s.logger.Warn("failed to write session store", "path", s.sessionStorePath, "error", err)
		}
		return
	}
	if err := os.Rename(tmp, s.sessionStorePath); err != nil && s.logger != nil {
		s.logger.Warn("failed to replace session store", "path", s.sessionStorePath, "error", err)
	}
}

// newSessionID returns a cryptographically-random opaque session id, or empty
// string if the system RNG fails (the caller must treat that as an error and
// refuse to create a session rather than fall back to a predictable value).
func newSessionID() string {
	b := make([]byte, sessionIDBytes)
	if _, err := crand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// createUserSession registers a new per-user session and returns its opaque id.
// Returns "" if a secure id could not be generated.
func (s *Server) createUserSession(username, role string) string {
	id := newSessionID()
	if id == "" {
		return ""
	}
	now := time.Now()
	s.sessionMu.Lock()
	// Opportunistically reap expired sessions so abandoned logins (browser
	// closed without logout) don't accumulate forever. lookupSession only
	// evicts on access, so a login-time sweep bounds the map to live sessions.
	for k, sess := range s.userSessions {
		if now.After(sess.ExpiresAt) {
			delete(s.userSessions, k)
		}
	}
	s.userSessions[id] = &userSession{
		Username:  username,
		Role:      role,
		ExpiresAt: now.Add(sessionTTL),
	}
	s.persistSessionsLocked()
	s.sessionMu.Unlock()
	return id
}

// ActiveSessionUsernames returns the DISTINCT GitHub usernames that have at
// least one live (non-expired) session on this hive right now. It is reported to
// the hub in the heartbeat so the hub can accumulate per-user "time in hive" by
// crediting each live user the inter-beat interval.
//
// NON-SECRET BY CONSTRUCTION: it returns bare usernames only — never a session
// id, the OAuth token (which the session never holds; see userSession's doc), or
// even the role. The result is sorted so the heartbeat payload is stable and does
// not churn a diff on every beat.
func (s *Server) ActiveSessionUsernames() []string {
	now := time.Now()
	seen := make(map[string]struct{})
	s.sessionMu.RLock()
	for _, sess := range s.userSessions {
		if sess == nil || now.After(sess.ExpiresAt) {
			continue
		}
		if sess.Username != "" {
			seen[sess.Username] = struct{}{}
		}
	}
	s.sessionMu.RUnlock()
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// lookupSession resolves a session id to its user session, dropping and
// rejecting expired sessions. Returns nil if unknown or expired.
func (s *Server) lookupSession(id string) *userSession {
	if id == "" {
		return nil
	}
	s.sessionMu.RLock()
	sess, ok := s.userSessions[id]
	s.sessionMu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(sess.ExpiresAt) {
		s.sessionMu.Lock()
		delete(s.userSessions, id)
		s.persistSessionsLocked()
		s.sessionMu.Unlock()
		return nil
	}
	return sess
}

// deleteSession removes a single session (logout affects only that session).
func (s *Server) deleteSession(id string) {
	if id == "" {
		return
	}
	s.sessionMu.Lock()
	delete(s.userSessions, id)
	s.persistSessionsLocked()
	s.sessionMu.Unlock()
}

// sessionFromRequest resolves the current request's per-user session from its
// hive_session cookie, if any. Returns nil when there is no valid session.
func (s *Server) sessionFromRequest(r *http.Request) *userSession {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	return s.lookupSession(c.Value)
}

// setSessionCookie writes the per-user session cookie for the given id.
func setSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   sessionCookieMaxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// setTerminalAssertionCookie mints a short-lived, HMAC-signed {user,hive,role,exp}
// assertion (hub.MintTerminalAssertion) and writes it as the hive_terminal_assertion
// cookie. This is the C3 follow-up upgrade over the static per-hive username
// allowlist: instead of the proxy consulting a list injected at PROVISION time,
// it now verifies a FRESH, EXPIRING, role-carrying grant the spoke minted for
// THIS user on THIS hive at LOGIN time.
//
// Called right after a per-user session is established (device-flow login and
// hub SSO handoff), where {username, role} are already authoritatively resolved
// against this hive's own allowlist. A no-op (no cookie) when the hive has no
// terminal signing key (not hub-hosted / no derivable key) or no HiveID — in that
// case the proxy simply falls back to the #2756 static-allowlist / ingress path,
// so there is no regression.
//
// The signing key is the SPOKE-LOCAL, SYMMETRIC terminal key (hub.TerminalSigningKey):
// the spoke mints here and the proxy verifies on the same spoke, so this is NOT
// the (now-asymmetric, C2 #2761) SSO signing path — it is decoupled from it.
//
// The cookie is Path=/terminal-scoped, HttpOnly and Secure — it is a bearer
// grant for a shell, so it never reaches page JS and never travels in the clear.
// It is deliberately NOT Domain=.hive.kubestellar.io: unlike the hub-wide
// hive_hub_user cookie (which every hive's domain receives and which was the root
// of C3), this assertion is bound to THIS hive by its signed HiveID claim AND
// only sent to this hive's host.
func (s *Server) setTerminalAssertionCookie(w http.ResponseWriter, r *http.Request, username, role string) {
	key := hub.TerminalSigningKey()
	if key == "" {
		return
	}
	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}
	if hiveID == "" {
		return
	}
	tok := hub.MintTerminalAssertion(key, username, role, hiveID, time.Now())
	if tok == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     terminalAssertionCookieName,
		Value:    tok,
		Path:     "/terminal",
		MaxAge:   int(terminalAssertionCookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// terminalAssertionCookieMaxAge caps how long the browser retains the assertion
// cookie. It matches the signed assertion's own TTL (hub.terminalAssertionTTL,
// 15 min): the cookie's lifetime and the token's expiry should agree so a stale
// cookie is dropped by the browser at roughly the same time the proxy would
// reject it as expired anyway. The proxy's exp check is authoritative regardless.
const terminalAssertionCookieMaxAge = 15 * time.Minute

// clearSessionCookie expires the per-user session cookie on the client.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
