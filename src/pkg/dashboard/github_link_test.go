package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Spoke side of the GitHub login link (#19992).

const (
	linkOIDCUser = "custom:alice-sub"
	linkGitHub   = "alice-gh"
)

func linkSSOServer(t *testing.T) *Server {
	t.Helper()
	s := newSSOServer(t, false, "shared-secret-token", linkOIDCUser+":read-write", "octo-owner:owner")
	s.sessionStorePath = filepath.Join(t.TempDir(), "dashboard-sessions.json")
	return s
}

func linkedSSOToken(t *testing.T, username string, link spoke.SSOGitHubLink) string {
	t.Helper()
	tok := spoke.MintSSOTokenWithGitHubLink(spoke.SSOSigningSeedFromMaster(testHubSecret), username, config.RoleOwner, testHiveID, link, time.Now())
	if tok == "" {
		t.Fatal("failed to mint handoff token")
	}
	return tok
}

func sessionFor(t *testing.T, s *Server, w *httptest.ResponseRecorder) *userSession {
	t.Helper()
	c := sessionCookie(w)
	if c == nil {
		t.Fatalf("no session cookie (status %d, body %s)", w.Code, w.Body.String())
	}
	sess := s.lookupSession(c.Value)
	if sess == nil {
		t.Fatal("session not found")
	}
	return sess
}

// A proven link rides on the session, is what contributor features see, and
// does not change the role the allowlist gives the person's own identity.
func TestGitHubLinkSSO_ProvenLinkSetsContributorIdentityOnly(t *testing.T) {
	s := linkSSOServer(t)
	w := ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: time.Now().Add(-time.Hour)}))
	sess := sessionFor(t, s, w)
	if sess.Username != linkOIDCUser || sess.GitHubLogin != linkGitHub {
		t.Fatalf("session = %+v", sess)
	}
	// The hub asked for owner; the allowlist says read-write; the link changes
	// nothing about that.
	if sess.Role != config.RoleReadWrite {
		t.Fatalf("role = %q, want read-write", sess.Role)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/contribute/me", nil)
	r.AddCookie(sessionCookie(w))
	if got := s.resolveViewerUsername(r); got != linkGitHub {
		t.Fatalf("resolveViewerUsername = %q, want %q", got, linkGitHub)
	}
	if got := s.resolveContributeCaller(r); got != linkGitHub {
		t.Fatalf("resolveContributeCaller = %q, want %q", got, linkGitHub)
	}
	if !strings.Contains(readAuditTail(t, s), auditGitHubLinkSession) {
		t.Fatal("link session not audited")
	}
}

// Without a link, or with a link on a GitHub sign-in, the session is unchanged.
func TestGitHubLinkSSO_NoLinkOrGitHubIdentityKeepsUsername(t *testing.T) {
	s := linkSSOServer(t)
	w := ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{}))
	if sess := sessionFor(t, s, w); sess.GitHubLogin != "" {
		t.Fatalf("no-link session carries %q", sess.GitHubLogin)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/contribute/me", nil)
	r.AddCookie(sessionCookie(w))
	if got := s.resolveViewerUsername(r); got != linkOIDCUser {
		t.Fatalf("resolveViewerUsername = %q", got)
	}

	// A GitHub sign-in has nothing to link: a carried link is ignored, so an
	// owner's GitHub identity can never be swapped for another login.
	w = ssoGet(t, s, "token="+linkedSSOToken(t, "octo-owner", spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: time.Now()}))
	if sess := sessionFor(t, s, w); sess.GitHubLogin != "" || sess.Username != "octo-owner" {
		t.Fatalf("GitHub identity session = %+v", sess)
	}
}

func TestSessionGitHubLink_Filters(t *testing.T) {
	s := linkSSOServer(t)
	now := time.Now()
	cases := []struct {
		name     string
		username string
		link     spoke.SSOGitHubLink
		want     string
	}{
		{"proven", linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: now}, linkGitHub},
		{"no time", linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub}, ""},
		{"invalid login", linkOIDCUser, spoke.SSOGitHubLink{Login: "bad login", LinkedAt: now}, ""},
		{"github identity", "octo-owner", spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: now}, ""},
		{"canonical github identity", "github:octo-owner", spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: now}, ""},
		{"contributor-id-shaped login", linkOIDCUser, spoke.SSOGitHubLink{Login: "c-0123456789ab", LinkedAt: now}, ""},
		{"proof too old", linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: now.Add(-githubLinkMaxProofAge - time.Hour)}, ""},
	}
	for _, tc := range cases {
		if got := s.sessionGitHubLink(tc.username, tc.link); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func revokeReq(t *testing.T, s *Server, body string, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/github-link/revoke", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if owner {
		r.Header.Set("X-Hive-Role", config.RoleOwner)
		r.Header.Set(ownerRoleVerifiedHeader, "true")
		r.Header.Set("X-Hive-User", "octo-owner")
	}
	w := httptest.NewRecorder()
	s.handleGitHubLinkRevoke(w, r)
	return w
}

// An owner can revoke but never create a link on the spoke.
func TestGitHubLinkRevoke_OwnerAssertedLinkIsRefused(t *testing.T) {
	s := linkSSOServer(t)
	w := revokeReq(t, s, `{"identity":"`+linkOIDCUser+`","login":"`+linkGitHub+`"}`, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("owner-asserted link = %d, want 403", w.Code)
	}
	if !strings.Contains(readAuditTail(t, s), auditGitHubLinkRefused) {
		t.Fatal("refusal not audited")
	}
	// Nothing about the identity changed: a later proven SSO still links.
	w = ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: time.Now()}))
	if sess := sessionFor(t, s, w); sess.GitHubLogin != linkGitHub {
		t.Fatalf("refused request changed state: %+v", sess)
	}
}

func TestGitHubLinkRevoke_OwnerOnlyAndValidated(t *testing.T) {
	s := linkSSOServer(t)
	if w := revokeReq(t, s, `{"identity":"`+linkOIDCUser+`"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner revoke = %d, want 403", w.Code)
	}
	if w := revokeReq(t, s, `{}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("empty identity = %d, want 400", w.Code)
	}
	if w := revokeReq(t, s, `not json`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d, want 400", w.Code)
	}
}

// Revoking clears live sessions and keeps the link off later sessions until a
// newer proof arrives.
func TestGitHubLinkRevoke_ClearsSessionsUntilReproven(t *testing.T) {
	s := linkSSOServer(t)
	proven := time.Now().Add(-time.Hour).Truncate(time.Second)
	w := ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: proven}))
	cookie := sessionCookie(w)

	rw := revokeReq(t, s, `{"identity":"`+linkOIDCUser+`"}`, true)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"sessions_cleared":1`) || !strings.Contains(rw.Body.String(), linkGitHub) {
		t.Fatalf("revoke = %d %s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(readAuditTail(t, s), auditGitHubLinkRevoked) {
		t.Fatal("revoke not audited")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/contribute/me", nil)
	r.AddCookie(cookie)
	if got := s.resolveViewerUsername(r); got != linkOIDCUser {
		t.Fatalf("after revoke viewer = %q, want own identity", got)
	}

	// The same proof again is still revoked.
	w = ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: proven}))
	if sess := sessionFor(t, s, w); sess.GitHubLogin != "" {
		t.Fatal("revoked link came back without a new proof")
	}
	// The revocation survives a restart of the list.
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.sessionStorePath), githubLinkRevocationsFile)); err != nil {
		t.Fatalf("revocation not persisted: %v", err)
	}
	s.githubLinkRevokes = githubLinkRevocations{}
	if !s.githubLinkRevoked(linkOIDCUser, linkGitHub, proven) {
		t.Fatal("revocation lost on reload")
	}
	// A newer proof on the hub restores it.
	w = ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: time.Now().Add(time.Minute)}))
	if sess := sessionFor(t, s, w); sess.GitHubLogin != linkGitHub {
		t.Fatal("a newer proof must restore the link")
	}
}

// With no live session at revoke time, the revocation covers any login.
func TestGitHubLinkRevoke_NoLiveSessionCoversAnyLogin(t *testing.T) {
	s := linkSSOServer(t)
	if w := revokeReq(t, s, `{"identity":"`+linkOIDCUser+`"}`, true); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d", w.Code)
	}
	if !s.githubLinkRevoked(linkOIDCUser, "any-login", time.Now().Add(-time.Minute)) {
		t.Fatal("login-less revocation must cover any login proven before it")
	}
	if s.githubLinkRevoked("custom:other", "any-login", time.Now().Add(-time.Minute)) {
		t.Fatal("revocation leaked to another identity")
	}
}

// Without a session store the list is in memory only.
func TestGitHubLinkRevocations_InMemoryWithoutStore(t *testing.T) {
	s := linkSSOServer(t)
	s.sessionStorePath = ""
	if err := s.recordGitHubLinkRevocation(linkOIDCUser, githubLinkRevocation{Login: linkGitHub, RevokedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !s.githubLinkRevoked(linkOIDCUser, linkGitHub, time.Now().Add(-time.Minute)) {
		t.Fatal("in-memory revocation not honoured")
	}
	if got := s.createUserSessionWithGitHubLogin(linkOIDCUser, config.RoleRead, "", time.Time{}); got == "" {
		t.Fatal("session without link not created")
	}
}

// readAuditTail returns the spoke's audit log content.
func readAuditTail(t *testing.T, s *Server) string {
	t.Helper()
	var b strings.Builder
	for _, e := range s.audit.Recent(auditTailEntries) {
		b.WriteString(e.Action)
		b.WriteString(" ")
		b.WriteString(e.Detail)
		b.WriteString("\n")
	}
	return b.String()
}

const auditTailEntries = 50

// A session honours its link only for sessionGitHubLinkTTL after the SSO
// handoff that attached it, so a hub unlink reaches it on the next handoff.
func TestGitHubLinkSession_LinkExpiresWithinTTL(t *testing.T) {
	now := time.Now()
	sess := &userSession{Username: linkOIDCUser, GitHubLogin: linkGitHub, GitHubLinkSetAt: now}
	if got := sess.activeGitHubLogin(now.Add(time.Minute)); got != linkGitHub {
		t.Fatalf("fresh link = %q", got)
	}
	if got := sess.activeGitHubLogin(now.Add(sessionGitHubLinkTTL + time.Minute)); got != "" {
		t.Fatalf("stale link still honoured: %q", got)
	}
	if got := (&userSession{GitHubLogin: linkGitHub}).activeGitHubLogin(now); got != "" {
		t.Fatal("a link with no attach time must not be honoured")
	}
	var nilSess *userSession
	if nilSess.activeGitHubLogin(now) != "" {
		t.Fatal("nil session")
	}
}

// A revoke records the hub proof time it revokes, so a hub clock running
// ahead of the spoke cannot bring the same proof back, while a genuinely
// newer proof can.
func TestGitHubLinkRevoke_ComparesHubProofTimesNotClocks(t *testing.T) {
	s := linkSSOServer(t)
	// The hub clock is an hour ahead of the spoke: its proof time is in the
	// spoke's future.
	proof := time.Now().Add(time.Hour).Truncate(time.Second)
	ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: proof}))
	if w := revokeReq(t, s, `{"identity":"`+linkOIDCUser+`"}`, true); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d", w.Code)
	}
	w := ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: proof}))
	if sess := sessionFor(t, s, w); sess.GitHubLogin != "" {
		t.Fatal("the revoked proof came back because the hub clock is ahead")
	}
	w = ssoGet(t, s, "token="+linkedSSOToken(t, linkOIDCUser, spoke.SSOGitHubLink{Login: linkGitHub, LinkedAt: proof.Add(time.Second)}))
	if sess := sessionFor(t, s, w); sess.GitHubLogin != linkGitHub || !sess.GitHubLinkedAt.Equal(proof.Add(time.Second)) {
		t.Fatalf("a newer proof must restore the link: %+v", sess)
	}
}
