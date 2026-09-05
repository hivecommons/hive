package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	hub "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Tests for the audit-F4 residual: the terminal assertion must be renewable from
// the SPOKE-LOCAL session alone, so the domain-scoped hive_hub_user cookie stops
// being the thing that keeps /terminal working past the assertion's 15-minute TTL.
//
// Each negative test below is written so that it FAILS against the unfixed tree
// (no renewal route: the mux 404s / writes no cookie) and passes after.

// newRenewServer builds a hosted-shaped spoke with a terminal signing key and a
// HiveID, which is what setTerminalAssertionCookie requires to mint at all.
func newRenewServer(t *testing.T, hiveID string, authorized ...string) *Server {
	t.Helper()
	t.Setenv(hub.EnvTerminalKey, "renew-test-terminal-key")
	s := newFullServer(t)
	s.deps.Config.HiveID = hiveID
	s.deps.Config.Dashboard.AuthorizedUsers = authorized
	return s
}

// assertionCookie pulls the minted assertion out of a response, if present.
func assertionCookie(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == terminalAssertionCookieName {
			return c
		}
	}
	return nil
}

// TestRenewAssertion_SessionOnly_NoHubCookie is the CORE regression: a live
// spoke session, with NO hive_hub_user cookie anywhere in the request, must yield
// a freshly-minted, valid, this-hive assertion.
//
// This is the property that makes the hub cookie removable later. Against the
// unfixed tree the route does not exist, so no cookie is set and this fails.
func TestRenewAssertion_SessionOnly_NoHubCookie(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	sid := s.createUserSession("alice", config.RoleOwner)

	req := httptest.NewRequest("POST", renewTerminalAssertionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	// Deliberately NO hive_hub_user cookie: that is the whole point.
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("renew from a valid spoke session = %d, want 204 (the hub cookie must not be required)", w.Code)
	}
	c := assertionCookie(w.Result())
	if c == nil {
		t.Fatal("renew minted no hive_terminal_assertion cookie")
	}
	user, role, err := hub.VerifyTerminalAssertion(hub.TerminalSigningKey(), c.Value, "hosted-alpha", time.Now())
	if err != nil {
		t.Fatalf("renewed assertion does not verify: %v", err)
	}
	if user != "alice" || role != config.RoleOwner {
		t.Fatalf("renewed assertion = user %q role %q, want alice/owner", user, role)
	}
}

// TestRenewAssertion_CookieStaysHostOnlyAndPathScoped guards the property that
// makes this credential spoke-scoped in the first place. A renewed assertion that
// picked up Domain=.hive.kubestellar.io would re-create the exact leak F4 is
// about — every sibling tenant would receive a terminal grant.
func TestRenewAssertion_CookieStaysHostOnlyAndPathScoped(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	sid := s.createUserSession("alice", config.RoleOwner)

	req := httptest.NewRequest("POST", renewTerminalAssertionPath, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	c := assertionCookie(w.Result())
	if c == nil {
		t.Fatal("renew minted no assertion cookie")
	}
	if c.Domain != "" {
		t.Fatalf("renewed assertion is domain-scoped (Domain=%q) — every sibling tenant would receive a terminal grant", c.Domain)
	}
	if c.Path != "/terminal" {
		t.Errorf("renewed assertion Path = %q, want /terminal", c.Path)
	}
	if !c.HttpOnly || !c.Secure {
		t.Errorf("renewed assertion lost hardening: HttpOnly=%v Secure=%v", c.HttpOnly, c.Secure)
	}
}

// TestRenewAssertion_RequiresSession is the fail-closed control: an unauthenticated
// caller must get 401 and NO cookie. A renewal endpoint that minted a terminal
// grant for anyone who asked would be a worse hole than the one being closed.
func TestRenewAssertion_RequiresSession(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")

	req := httptest.NewRequest("POST", renewTerminalAssertionPath, nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("renew with no session = %d, want 401", w.Code)
	}
	if c := assertionCookie(w.Result()); c != nil && c.Value != "" {
		t.Fatal("renew minted an assertion for an unauthenticated caller")
	}
}

// TestRenewAssertion_IgnoresHubCookieAsCredential proves the endpoint cannot be
// driven by the very credential F4 is about. A hostile sibling that RECEIVED a
// victim's hive_hub_user must not be able to trade it for a terminal grant here.
func TestRenewAssertion_IgnoresHubCookieAsCredential(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")

	req := httptest.NewRequest("POST", renewTerminalAssertionPath, nil)
	// A perfectly good hub-wide cookie, but no spoke session.
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "alice"})
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("renew authenticated by hive_hub_user alone = %d, want 401", w.Code)
	}
	if c := assertionCookie(w.Result()); c != nil && c.Value != "" {
		t.Fatal("hive_hub_user alone minted a terminal assertion — the hub cookie must not be a credential here")
	}
}

// TestRenewAssertion_ReResolvesRoleFromAllowlist is the revocation control. A
// self-renewing grant that froze its role at login would let a downgraded user
// keep shell access forever — strictly worse than the 15-minute expiry it
// replaces. The renewed assertion must carry the CURRENT allowlist role.
func TestRenewAssertion_ReResolvesRoleFromAllowlist(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	// Session was created when alice was an owner.
	sid := s.createUserSession("alice", config.RoleOwner)
	// She has since been downgraded to read-only on this hive.
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"alice:read"}

	req := httptest.NewRequest("POST", renewTerminalAssertionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	c := assertionCookie(w.Result())
	if c == nil {
		t.Fatal("renew minted no assertion cookie")
	}
	_, role, err := hub.VerifyTerminalAssertion(hub.TerminalSigningKey(), c.Value, "hosted-alpha", time.Now())
	if err != nil {
		t.Fatalf("renewed assertion does not verify: %v", err)
	}
	if role != config.RoleRead {
		t.Fatalf("renewed assertion role = %q, want read — a downgrade must take effect at renewal, not be frozen at login", role)
	}
}

// TestRenewAssertion_BoundToThisHive proves the renewed grant cannot open a
// terminal on a sibling hive: verification against another hive's ID must fail.
func TestRenewAssertion_BoundToThisHive(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	sid := s.createUserSession("alice", config.RoleOwner)

	req := httptest.NewRequest("POST", renewTerminalAssertionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	c := assertionCookie(w.Result())
	if c == nil {
		t.Fatal("renew minted no assertion cookie")
	}
	if _, _, err := hub.VerifyTerminalAssertion(hub.TerminalSigningKey(), c.Value, "hosted-victim", time.Now()); err == nil {
		t.Fatal("an assertion renewed on hosted-alpha verified against hosted-victim — the hive binding is broken")
	}
}

// --- POSITIVE CONTROLS -------------------------------------------------------
//
// A gate broken to reject everything must not be able to pass this suite. These
// assert the legitimate flows still work.

// TestPositiveControl_SSOHandoffStillMintsAssertion: the SSO handoff (the flow
// ~62 live tenants use to open a hosted spoke) must still establish a session and
// still mint the terminal assertion.
func TestPositiveControl_SSOHandoffStillMintsAssertion(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	sid := s.createUserSession("alice", config.RoleOwner)
	if sid == "" {
		t.Fatal("SSO/login session creation broke")
	}
	if got := s.lookupSession(sid); got == nil || got.Username != "alice" {
		t.Fatal("session lookup broke — hosted login would fail")
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	s.setTerminalAssertionCookie(w, req, "alice", config.RoleOwner)
	if assertionCookie(w.Result()) == nil {
		t.Fatal("login/SSO path no longer mints a terminal assertion — /terminal would break for every hosted tenant")
	}
}

// TestPositiveControl_LegitimateTerminalGrantVerifies: a normally-minted
// assertion must still verify for the right user, hive and role. If this passes
// only because everything is denied, the suite is worthless.
func TestPositiveControl_LegitimateTerminalGrantVerifies(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	s.setTerminalAssertionCookie(w, req, "alice", config.RoleOwner)

	c := assertionCookie(w.Result())
	if c == nil {
		t.Fatal("no assertion minted")
	}
	user, role, err := hub.VerifyTerminalAssertion(hub.TerminalSigningKey(), c.Value, "hosted-alpha", time.Now())
	if err != nil {
		t.Fatalf("a legitimate terminal grant failed to verify: %v", err)
	}
	if user != "alice" || role != config.RoleOwner {
		t.Fatalf("legitimate grant = %q/%q, want alice/owner", user, role)
	}
}

// TestPositiveControl_RenewedAssertionIsAcceptedNotJustIssued closes the loop:
// the renewed cookie must actually authorize, with the same key the proxy
// resolves, for the same user — not merely be a well-formed string.
func TestPositiveControl_RenewedAssertionIsAcceptedNotJustIssued(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha", "alice:owner")
	sid := s.createUserSession("alice", config.RoleOwner)

	req := httptest.NewRequest("POST", renewTerminalAssertionPath, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	c := assertionCookie(w.Result())
	if c == nil {
		t.Fatal("renew minted no assertion")
	}
	user, role, err := hub.VerifyTerminalAssertion(hub.TerminalSigningKey(), c.Value, "hosted-alpha", time.Now())
	if err != nil {
		t.Fatalf("renewed assertion rejected by the same verifier the proxy mirrors: %v", err)
	}
	// TERMINAL_ROLES in proxy/server.js accepts owner / read-write.
	if user != "alice" || (role != config.RoleOwner && role != config.RoleReadWrite) {
		t.Fatalf("renewed assertion would not open a terminal: user=%q role=%q", user, role)
	}
}
