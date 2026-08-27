package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression tests for #4299 (3rd report of the "owner access required" class,
// after #4081/#4082 and #4134): on a direct-route spoke, the role resolved at
// login was frozen into the 30-day persisted session, so a Manage Access OWNER
// grant delivered by the hub heartbeat (which live-updates
// cfg.Dashboard.AuthorizedUsers) never took effect for an already-signed-in
// user — every owner-gated mutation kept answering 403 "owner access required"
// until they signed out and back in. Downgrades and revocations were equally
// frozen.
//
// These tests run PUT /api/config/governor/budget through the FULL middleware
// stack (authenticate → roleEnforcement → mux), exactly as a browser request
// arrives, with a session whose stored role deliberately DISAGREES with the
// live allowlist — the allowlist must win, in both directions.

// seedBudget4299 stores a known-good budget so a partial update validates.
func seedBudget4299(s *Server) {
	s.deps.Config.Governor.Budget.TotalTokens = 1000
	s.deps.Config.Governor.Budget.PeriodDays = 7
	s.deps.Config.Governor.Budget.CriticalPct = 90
}

func putBudget4299(s *Server, sid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/budget",
		strings.NewReader(`{"totalTokens":50000}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// The dwaddington scenario: logged in while read-write, THEN granted owner via
// Manage Access (heartbeat updates the allowlist in place). The very next
// request must already be an owner — no re-login, no session mutation.
func TestBudgetSave4299_GithubGrantedOwnerWithStaleSession(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:read-write")
	seedBudget4299(s)
	sid := s.createUserSession("dwaddington", "read-write")

	// The hub heartbeat delivers the new grant.
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "dwaddington:owner"}

	w := putBudget4299(s, sid)
	if w.Code != http.StatusOK {
		t.Fatalf("granted owner budget save = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 50000 {
		t.Fatalf("totalTokens = %d, want 50000 (save did not persist)", got)
	}
}

// Same scenario for a non-GitHub canonical identity: an ibmid-granted owner
// must pass the budget-save owner gate.
func TestBudgetSave4299_IbmidGrantedOwnerWithStaleSession(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "ibmid:310002BM0V:read")
	seedBudget4299(s)
	sid := s.createUserSession("ibmid:310002BM0V", "read")

	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "ibmid:310002BM0V:owner"}

	w := putBudget4299(s, sid)
	if w.Code != http.StatusOK {
		t.Fatalf("ibmid granted owner budget save = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// Legacy/canonical GitHub form mismatch: the hub may deliver the allowlist
// entry in canonical wire form ("github:<login>") while the device-flow login
// yields the bare login. They are the SAME user (the hub's legacy shim) and
// must match.
func TestBudgetSave4299_CanonicalAllowlistEntryMatchesBareLogin(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "github:dwaddington:owner")
	seedBudget4299(s)
	sid := s.createUserSession("dwaddington", "read-write")

	w := putBudget4299(s, sid)
	if w.Code != http.StatusOK {
		t.Fatalf("canonical-entry owner budget save = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// And the reverse form mix: bare allowlist entry, canonical session username
// (an SSO handoff carries the hub's cookie subject).
func TestBudgetSave4299_BareAllowlistEntryMatchesCanonicalLogin(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:owner")
	seedBudget4299(s)
	sid := s.createUserSession("github:dwaddington", "read")

	w := putBudget4299(s, sid)
	if w.Code != http.StatusOK {
		t.Fatalf("bare-entry owner budget save = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// The gate must be LIVE in both directions: a downgrade delivered after login
// takes owner away from a session that was minted as owner.
func TestBudgetSave4299_DowngradeBindsImmediately(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:owner")
	seedBudget4299(s)
	sid := s.createUserSession("dwaddington", "owner")

	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "dwaddington:read"}

	w := putBudget4299(s, sid)
	if w.Code != http.StatusForbidden {
		t.Fatalf("downgraded user budget save = %d, want 403; body=%q", w.Code, w.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 1000 {
		t.Fatalf("totalTokens = %d, want 1000 (denied save must not persist)", got)
	}
}

// A revocation must invalidate the session outright — a revoked user must not
// coast on a stale 30-day session at any role.
func TestBudgetSave4299_RevocationBindsImmediately(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:owner")
	seedBudget4299(s)
	sid := s.createUserSession("dwaddington", "owner")

	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner"}

	w := putBudget4299(s, sid)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked user budget save = %d, want 401; body=%q", w.Code, w.Body.String())
	}
}

// Every owner-gated mutation goes through the SAME shared chain
// (authenticate's liveSessionRole → requireOwnerRole reading X-Hive-Role +
// the server-set verification marker), so proving the chain at the middleware
// boundary covers the whole class, not just the budget endpoint. A stale
// read-write session with a live owner grant must arrive at ANY handler as a
// verified owner.
func TestLiveRole4299_AuthenticateInjectsLiveOwnerForAllHandlers(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:read-write")
	sid := s.createUserSession("dwaddington", "read-write")
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "dwaddington:owner"}

	var sawUser, sawRole, sawMarker string
	handler := s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUser = r.Header.Get("X-Hive-User")
		sawRole = r.Header.Get("X-Hive-Role")
		sawMarker = r.Header.Get(ownerRoleVerifiedHeader)
		w.WriteHeader(http.StatusOK)
	}))

	// requireOwnerRole gates every owner-only endpoint on exactly these two
	// headers; asserting them here therefore covers pause/resume, config
	// download, self-upgrade, governor settings, packs, escalation, backup —
	// every requireOwnerRole caller — in one place.
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/budget", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if sawUser != "dwaddington" {
		t.Errorf("X-Hive-User = %q, want dwaddington", sawUser)
	}
	if sawRole != "owner" {
		t.Errorf("X-Hive-Role = %q, want owner (live allowlist role, not the frozen session role)", sawRole)
	}
	if sawMarker != "true" {
		t.Errorf("%s = %q, want true (requireOwnerRole demands the server-set marker)", ownerRoleVerifiedHeader, sawMarker)
	}
}
