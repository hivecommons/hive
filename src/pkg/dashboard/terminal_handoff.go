package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
)

const (
	terminalHandoffPath = "/api/terminal/handoff"
	terminalHandoffTTL  = 60 * time.Second
)

type terminalHandoff struct {
	Username  string
	Role      string
	ExpiresAt time.Time
}

func isTerminalPath(path string) bool {
	return path == terminalPathPrefix || strings.HasPrefix(path, terminalPathPrefix+"/")
}

func terminalRoleAllowed(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case config.RoleOwner, config.RoleReadWrite:
		return true
	default:
		return false
	}
}

func (s *Server) handleCreateTerminalHandoff(w http.ResponseWriter, r *http.Request) {
	role := strings.TrimSpace(r.Header.Get("X-Hive-Role"))
	if role == "" {
		role = config.RoleOwner
	}
	if !terminalRoleAllowed(role) {
		http.Error(w, `{"error":"terminal access requires owner or read-write role"}`, http.StatusForbidden)
		return
	}
	user := requestUser(r)
	if !s.canMintTerminalAssertion() {
		http.Error(w, `{"error":"terminal handoff requires terminal signing key and hive id"}`, http.StatusServiceUnavailable)
		return
	}
	s.setTerminalAssertionCookie(w, r, user, role)
	code := s.createTerminalHandoff(user, role)
	if code == "" {
		http.Error(w, `{"error":"failed to create terminal handoff"}`, http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"code": code, "expires_in": "60"})
}

func (s *Server) canMintTerminalAssertion() bool {
	if hub.TerminalSigningKey() == "" {
		return false
	}
	return s.deps != nil && s.deps.Config != nil && s.deps.Config.HiveID != ""
}

func (s *Server) createTerminalHandoff(username, role string) string {
	code := newSessionID()
	if code == "" {
		return ""
	}
	now := time.Now()
	s.terminalHandoffMu.Lock()
	if s.terminalHandoffs == nil {
		s.terminalHandoffs = make(map[string]terminalHandoff)
	}
	for k, h := range s.terminalHandoffs {
		if now.After(h.ExpiresAt) {
			delete(s.terminalHandoffs, k)
		}
	}
	s.terminalHandoffs[code] = terminalHandoff{Username: username, Role: role, ExpiresAt: now.Add(terminalHandoffTTL)}
	s.terminalHandoffMu.Unlock()
	return code
}

func (s *Server) redeemTerminalHandoff(code string) (username, role string, ok bool) {
	if code == "" {
		return "", "", false
	}
	s.terminalHandoffMu.Lock()
	h, found := s.terminalHandoffs[code]
	if found {
		delete(s.terminalHandoffs, code)
	}
	s.terminalHandoffMu.Unlock()
	if !found || time.Now().After(h.ExpiresAt) || h.Username == "" {
		return "", "", false
	}
	if h.Role == "" {
		h.Role = config.RoleRead
	}
	return h.Username, h.Role, true
}

func (s *Server) terminalAssertionFromRequest(r *http.Request) (username, role string, ok bool) {
	c, err := r.Cookie(terminalAssertionCookieName)
	if err != nil || c.Value == "" {
		return "", "", false
	}
	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}
	user, terminalRole, err := hub.VerifyTerminalAssertion(hub.TerminalSigningKey(), c.Value, hiveID, time.Now())
	if err != nil || user == "" {
		return "", "", false
	}
	if terminalRole == "" {
		terminalRole = config.RoleRead
	}
	return user, terminalRole, true
}

func writeTerminalRoleForbidden(w http.ResponseWriter, r *http.Request) {
	msg := "terminal access requires owner or read-write role"
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"`+msg+`"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, msg, http.StatusForbidden)
}

func writeQueryTokenRejected(w http.ResponseWriter, r *http.Request) {
	msg := "query-string dashboard token authentication is no longer supported; use the Authorization header, sign in with a session cookie, or upgrade the hub/client that generated this link"
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"`+msg+`"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, msg, http.StatusUnauthorized)
}

func redactedRequestURI(u *url.URL) string {
	if u == nil {
		return ""
	}
	clone := *u
	q := clone.Query()
	changed := false
	for _, key := range []string{"token", "code"} {
		if _, ok := q[key]; ok {
			q.Set(key, "[redacted]")
			changed = true
		}
	}
	if changed {
		clone.RawQuery = q.Encode()
	}
	return clone.RequestURI()
}
