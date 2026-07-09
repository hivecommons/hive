package dashboard

import (
	crand "crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// userSession is a per-user server-side session created after a successful,
// AUTHORIZED device-flow login on a direct-route spoke. Sessions are keyed by a
// random opaque id held in the client's hive_session cookie, so two different
// GitHub users get two distinct sessions and each request resolves to the user
// that owns ITS cookie — never a single shared identity.
//
// The GitHub OAuth token is intentionally NOT stored on the session; the token
// lives only where it is needed (SetUserClient / userTokenPath) and is never
// exposed through the session. The session carries only identity + role.
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
	s.sessionMu.Unlock()
	return id
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
