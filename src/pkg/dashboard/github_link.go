package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Spoke side of the GitHub login link (hivecommons/hive#19992).
//
// The hub proves a link (the person completes GitHub OAuth from their own
// OpenID Connect session) and carries it in the signed SSO handoff token. The
// spoke stores the login on the session it mints, and uses it ONLY where the
// spoke keys a person by GitHub login for contributor features
// (resolveViewerUsername, and through it resolveContributeCaller): the person
// then has exactly the contributor access that GitHub login already has here.
//
// The link changes nothing else. The session's Username and Role stay the
// person's own sign-in identity and allowlist role, so the link cannot raise a
// role, and the owner-authored merge checks (which match PR authors against
// authorized_users) never read it: a linked owner's PRs count as owner-authored
// only when authorized_users already lists that GitHub login as an owner.
//
// An owner can revoke a link here but cannot create one: the revoke endpoint
// takes only the identity, and a request that names a login is refused.

const (
	// githubLinkRevocationsFile sits next to the session store, in the same
	// trust domain.
	githubLinkRevocationsFile = "github-link-revocations.json"

	// Audit actions.
	auditGitHubLinkSession = "github_link_session"
	auditGitHubLinkRevoked = "github_link_revoked"
	auditGitHubLinkRefused = "github_link_refused"
)

// githubLinkRevocation records an owner's revoke of one identity's link.
// A later proof on the hub (a LinkedAt after RevokedAt) restores it.
type githubLinkRevocation struct {
	Login     string    `json:"login"`
	RevokedAt time.Time `json:"revoked_at"`
	By        string    `json:"by"`
}

// githubLinkRevocations is the spoke's revocation list, keyed by identity.
type githubLinkRevocations struct {
	mu      sync.Mutex
	loaded  bool
	entries map[string]githubLinkRevocation
}

func (s *Server) githubLinkRevocationsPath() string {
	s.sessionMu.RLock()
	p := s.sessionStorePath
	s.sessionMu.RUnlock()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), githubLinkRevocationsFile)
}

func (s *Server) loadGitHubLinkRevocationsLocked() {
	rv := &s.githubLinkRevokes
	if rv.loaded {
		return
	}
	rv.loaded = true
	rv.entries = map[string]githubLinkRevocation{}
	path := s.githubLinkRevocationsPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &rv.entries)
	if rv.entries == nil {
		rv.entries = map[string]githubLinkRevocation{}
	}
}

// githubLinkRevoked reports whether an owner revoked identity's link to login
// at or after linkedAt.
func (s *Server) githubLinkRevoked(identity, login string, linkedAt time.Time) bool {
	rv := &s.githubLinkRevokes
	rv.mu.Lock()
	defer rv.mu.Unlock()
	s.loadGitHubLinkRevocationsLocked()
	e, ok := rv.entries[identity]
	// A revocation with no login (none was on a live session at the time)
	// covers whatever login the identity carries.
	if !ok || (e.Login != "" && !strings.EqualFold(e.Login, login)) {
		return false
	}
	return !linkedAt.After(e.RevokedAt)
}

// recordGitHubLinkRevocation stores a revocation and persists the list.
func (s *Server) recordGitHubLinkRevocation(identity string, e githubLinkRevocation) error {
	rv := &s.githubLinkRevokes
	rv.mu.Lock()
	defer rv.mu.Unlock()
	s.loadGitHubLinkRevocationsLocked()
	rv.entries[identity] = e
	path := s.githubLinkRevocationsPath()
	if path == "" {
		return nil
	}
	data, err := json.Marshal(rv.entries)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sessionGitHubLink returns the GitHub login an SSO handoff may attach to the
// session for username, or "". The link is dropped when the identity is
// itself a GitHub login (nothing to link), when the login is not a valid
// GitHub login, or when an owner revoked it after it was proven.
func (s *Server) sessionGitHubLink(username string, link spoke.SSOGitHubLink) string {
	login := strings.TrimSpace(link.Login)
	if login == "" || link.LinkedAt.IsZero() {
		return ""
	}
	if !strings.Contains(username, ":") {
		return ""
	}
	if !validGitHubUsername(login) {
		return ""
	}
	if s.githubLinkRevoked(username, login, link.LinkedAt) {
		return ""
	}
	return login
}

// createUserSessionWithGitHubLogin is createUserSession recording a proven
// GitHub login link on the session.
func (s *Server) createUserSessionWithGitHubLogin(username, role, githubLogin string) string {
	id := s.createUserSession(username, role)
	if id == "" || githubLogin == "" {
		return id
	}
	s.sessionMu.Lock()
	if sess := s.userSessions[id]; sess != nil {
		sess.GitHubLogin = githubLogin
		s.persistSessionsLocked()
	}
	s.sessionMu.Unlock()
	return id
}

// clearSessionGitHubLinks removes identity's link from every live session and
// reports how many sessions changed.
func (s *Server) clearSessionGitHubLinks(identity string) int {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	n := 0
	for _, sess := range s.userSessions {
		if sess != nil && sess.Username == identity && sess.GitHubLogin != "" {
			sess.GitHubLogin = ""
			n++
		}
	}
	if n > 0 {
		s.persistSessionsLocked()
	}
	return n
}

// handleGitHubLinkRevoke serves POST /api/auth/github-link/revoke with
// {"identity": "<provider>:<subject>"}. Owner-only. It removes the identity's
// GitHub login link from live sessions and keeps it off later SSO sessions
// until the person proves the link again on the hub. A body that names a
// login is an attempt to create or change a link and is refused.
func (s *Server) handleGitHubLinkRevoke(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Identity string `json:"identity"`
		Login    string `json:"login"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	identity := strings.TrimSpace(body.Identity)
	if strings.TrimSpace(body.Login) != "" {
		s.auditFromRequest(r, auditGitHubLinkRefused,
			auditDetail("identity", identity, "login", body.Login, "reason", "an owner cannot create a link"), "")
		jsonError(w, "A GitHub link is created only by the person completing GitHub sign-in on the hub. An owner can only revoke one.", http.StatusForbidden)
		return
	}
	if identity == "" {
		jsonError(w, "identity is required", http.StatusBadRequest)
		return
	}
	login := s.liveSessionGitHubLogin(identity)
	if err := s.recordGitHubLinkRevocation(identity, githubLinkRevocation{Login: login, RevokedAt: time.Now().UTC(), By: requestUser(r)}); err != nil {
		jsonError(w, "could not save the revocation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cleared := s.clearSessionGitHubLinks(identity)
	s.auditFromRequest(r, auditGitHubLinkRevoked, auditDetail("identity", identity, "login", login), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "identity": identity, "login": login, "sessions_cleared": cleared})
}

// liveSessionGitHubLogin is the GitHub login a live session of identity
// carries, or "".
func (s *Server) liveSessionGitHubLogin(identity string) string {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	now := time.Now()
	for _, sess := range s.userSessions {
		if sess != nil && sess.Username == identity && sess.GitHubLogin != "" && now.Before(sess.ExpiresAt) {
			return sess.GitHubLogin
		}
	}
	return ""
}
