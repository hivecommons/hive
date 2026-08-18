package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Audit F4 (CWE-352/CWE-346): hosted sibling-domain CSRF boundary.
//
// The hub session cookie is minted with Domain=.hive.kubestellar.io so that each
// tenant spoke's proxy can verify it. The consequence is that the browser
// attaches the victim's hub session to requests issued by ANY
// <id>.hive.kubestellar.io document — including one served by a hostile tenant,
// whose operator fully controls the script running there.
//
// isTrustedOrigin used to accept every suffix match of that shared domain, so
// the CSRF gate treated a hostile tenant's Origin as the hub's own. The audit
// demonstrated the end-to-end consequence by changing a victim hive's
// visibility from a sibling Origin. These tests reproduce that PoC at the
// handler boundary and pin it shut.
//
// WHY THESE TESTS EXIST IN THIS SHAPE: every finding in this audit round hid
// behind a green test that asserted the wrong thing — and this one literally
// did. saas_admin_csrf_test.go carried
// TestRequireAdminCSRFSiblingTenantOriginStillTrusted, which ASSERTED the
// vulnerable behaviour and t.Skip'd once it was fixed. So each test below is
// paired with a positive control proving the legitimate same-origin request is
// still ACCEPTED. A test that only checks rejection passes equally well against
// a handler that rejects everything.

// siblingOrigin is a hostile hosted tenant: a real, suffix-matching
// *.hive.kubestellar.io host whose operator is not the victim.
const siblingOrigin = "https://attacker-hive.hive.kubestellar.io"

// hubOrigin is the hub's own dashboard origin — the only legitimate author of a
// state-changing request.
const hubOrigin = "https://hive.kubestellar.io"

// visibilityReq builds the audit's PoC request: the exact mutation the report
// reproduced, carrying a genuinely VALID victim session cookie. Because the
// cookie is valid, a 403 can only come from the CSRF gate — not from the
// identity check, which would mask the finding.
func visibilityReq(t *testing.T, origin, username string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/saas/hives/victim-hive/visibility",
		strings.NewReader(`{"visibility":"public"}`))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.AddCookie(testAuthCookie(username))
	return setPathValue(r, "id", "victim-hive")
}

// TestVisibilityMutationFromSiblingOriginDenied is the direct regression for the
// audit's proof of concept. Against the unfixed code this returns 200/handler
// reached; it must now be refused 403 before the handler body runs.
func TestVisibilityMutationFromSiblingOriginDenied(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "victim")

	reached := false
	handler := s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	handler(rec, visibilityReq(t, siblingOrigin, "victim"))

	if reached {
		t.Fatal("F4 REGRESSION: a mutation from a hostile sibling tenant origin reached the handler — " +
			"the sibling holds the victim's parent-domain session cookie, so this is a cross-tenant write")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sibling-origin visibility mutation: code=%d, want 403 (CSRF); body=%q", rec.Code, rec.Body.String())
	}
}

// TestVisibilityMutationFromHubOriginAllowed is the POSITIVE CONTROL for the
// test above. Without it, a handler that refused every request — or a CSRF gate
// broken to always fail closed — would satisfy the regression and look fixed
// while the dashboard was entirely non-functional.
func TestVisibilityMutationFromHubOriginAllowed(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "victim")

	reached := false
	handler := s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	handler(rec, visibilityReq(t, hubOrigin, "victim"))

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("OVER-CORRECTION: the legitimate same-origin dashboard mutation was blocked: "+
			"code=%d reached=%v, want 200/true", rec.Code, reached)
	}
}

// TestSiblingOriginDeniedByRefererToo covers the second lane of isCSRFSafe: when
// no Origin header is present the gate falls back to Referer, and that fallback
// used the same suffix match. A hostile tenant can control its Referer just as
// easily as its Origin.
func TestSiblingOriginDeniedByRefererToo(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/api/saas/hives/victim-hive/visibility", nil)
	req.Header.Set("Referer", siblingOrigin+"/dashboard")
	if isCSRFSafe(req) {
		t.Fatal("F4 REGRESSION: sibling tenant trusted via the Referer fallback")
	}

	// Positive control: the hub's own Referer must still pass, or the dashboard
	// breaks for any browser that suppresses Origin.
	ok := httptest.NewRequest(http.MethodPut, "/api/saas/hives/victim-hive/visibility", nil)
	ok.Header.Set("Referer", hubOrigin+"/dashboard")
	if !isCSRFSafe(ok) {
		t.Fatal("OVER-CORRECTION: the hub's own Referer is no longer trusted")
	}
}

// TestSiblingOriginGetsNoCredentialedCORS covers the third lane. isTrustedOrigin
// did not only gate CSRF: the same predicate decided whether to reflect
// Access-Control-Allow-Origin together with Allow-Credentials. Reflecting a
// sibling there let a hostile tenant READ cross-tenant hub responses from
// script, which is the disclosure counterpart to the write PoC.
func TestSiblingOriginGetsNoCredentialedCORS(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	// OPTIONS preflight: short-circuits before any auth work, so the response
	// headers isolate exactly the CORS decision.
	req := httptest.NewRequest(http.MethodOptions, "/api/saas/hives/victim-hive/visibility", nil)
	req.Header.Set("Origin", siblingOrigin)
	rec := httptest.NewRecorder()
	s.handleToggleVisibility(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("F4 REGRESSION: reflected Access-Control-Allow-Origin=%q to a hostile sibling tenant "+
			"(with Allow-Credentials=%q) — that is a cross-tenant read channel",
			got, rec.Header().Get("Access-Control-Allow-Credentials"))
	}

	// Positive control: the hub's own origin must still get the CORS headers,
	// otherwise this test would pass against a build that emitted none at all.
	okReq := httptest.NewRequest(http.MethodOptions, "/api/saas/hives/victim-hive/visibility", nil)
	okReq.Header.Set("Origin", hubOrigin)
	okRec := httptest.NewRecorder()
	s.handleToggleVisibility(okRec, okReq)

	if got := okRec.Header().Get("Access-Control-Allow-Origin"); got != hubOrigin {
		t.Fatalf("OVER-CORRECTION: hub origin got Access-Control-Allow-Origin=%q, want %q", got, hubOrigin)
	}
}

// TestHostedLoginRedirectToSiblingStillWorks is the guard on what this fix
// deliberately did NOT narrow. Every hosted hive's ingress carries
// auth-signin: https://hive.kubestellar.io/login?redirect=$scheme://$http_host$request_uri
// so the ordinary "open my hive" sign-in arrives at the hub asking to be sent
// back to a sibling host. If the exact-origin tightening were applied to the
// redirect allowlist too, sign-in would break for all ~62 hosted tenants — a
// far more visible outage than the bug being fixed.
//
// Sending a browser TO a tenant is not the same capability as accepting a
// mutation FROM one: the tenant already controls that host.
func TestHostedLoginRedirectToSiblingStillWorks(t *testing.T) {
	if !isTrustedRedirectTarget("https://hosted-acme-web-xyz.hive.kubestellar.io/dashboard") {
		t.Fatal("hosted tenant login redirect rejected — this breaks the ingress auth-signin flow " +
			"for every hosted hive; the exact-origin fix must apply to CSRF/CORS only")
	}
	// Positive control: the redirect allowlist is still an allowlist.
	if isTrustedRedirectTarget("https://hive.kubestellar.io.evil.com/") {
		t.Fatal("redirect allowlist accepts a lookalike host — open redirect")
	}
}

// TestImpersonateCookieIsHostOnly pins the second half of F4: the admin's
// impersonation grant must not be broadcast to every tenant. Unlike
// hive_hub_user (which spokes must receive in order to verify it), nothing
// outside this package ever reads hive_hub_impersonate, so it costs nothing to
// scope it to the hub host alone.
func TestImpersonateCookieIsHostOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	setImpersonateCookie(rec, "admin|target|123.sig")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("mint should emit exactly one cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Domain != "" {
		t.Fatalf("F4 REGRESSION: impersonation cookie carries Domain=%q — it is transmitted to every "+
			"hosted tenant, handing the admin's live grant to ~62 untrusted third parties", c.Domain)
	}
	// Positive controls: dropping Domain must not have weakened anything else,
	// and the cookie must still actually be set.
	if c.Value == "" {
		t.Fatal("impersonation cookie value empty — the grant would never be established")
	}
	if !c.Secure || !c.HttpOnly || c.Path != "/" {
		t.Fatalf("impersonation cookie lost a hardening attribute: Secure=%v HttpOnly=%v Path=%q",
			c.Secure, c.HttpOnly, c.Path)
	}
}

// TestSessionCookieStillDomainScopedForSpokes pins the ONE part of F4 this
// change deliberately does not fix, so the decision is explicit in the suite
// rather than an unexplained omission.
//
// hive_hub_user MUST keep Domain=.hive.kubestellar.io: each spoke's Node proxy
// (src/proxy/server.js) independently verifies this cookie to authenticate the
// tenant dashboard and terminal, and it can only verify a cookie the browser
// sends it — which happens solely because of that Domain attribute. Making it
// host-only or __Host- would log every hosted tenant out fleet-wide.
//
// If you are here because you want the __Host- prefix the audit asked for: the
// prerequisite is a spoke-scoped session minted at SSO handoff, so the hub
// cookie no longer has to reach the spoke at all. Change this test when that
// lands, together with proxy/server.js — never on its own.
// Asserted against the real handler (handleLogout mints the same cookie shape
// as the OAuth callback, and needs no GitHub round-trip to exercise), so this
// tracks the production code rather than restating net/http's behaviour.
func TestSessionCookieStillDomainScopedForSpokes(t *testing.T) {
	s := newHandlerHub()
	rec := httptest.NewRecorder()
	s.handleLogout(rec, httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))

	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name != "hive_hub_user" {
			continue
		}
		found = true
		if c.Domain == "" {
			t.Fatal("hive_hub_user is no longer domain-scoped. Each spoke's Node proxy " +
				"(src/proxy/server.js) verifies this cookie and only receives it because of the " +
				"Domain attribute — host-only/__Host- logs out every hosted tenant fleet-wide. " +
				"See the F4 note in oauth.go before changing this.")
		}
		// The hardening that IS in place must not regress either.
		if !c.Secure || !c.HttpOnly {
			t.Errorf("hive_hub_user lost hardening: Secure=%v HttpOnly=%v", c.Secure, c.HttpOnly)
		}
	}
	if !found {
		t.Fatal("handleLogout emitted no hive_hub_user cookie")
	}
}

// TestImpersonateExitClearsLegacyDomainCookie guards the migration edge. A
// host-only Set-Cookie cannot delete a domain-scoped cookie — they are separate
// jar entries — so without an explicit legacy deletion, "Exit impersonation"
// would appear to work while the browser kept replaying the old domain-scoped
// grant to the hub until its TTL lapsed.
func TestImpersonateExitClearsLegacyDomainCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	setImpersonateCookie(rec, "")

	var clearedHostOnly, clearedLegacy bool
	for _, c := range rec.Result().Cookies() {
		if c.Name != impersonateCookieName || c.MaxAge >= 0 {
			continue
		}
		if c.Domain == "" {
			clearedHostOnly = true
		}
		// http.SetCookie serializes Domain=".hive.kubestellar.io" as
		// "Domain=hive.kubestellar.io" — RFC 6265 §5.2.3 says a leading dot is
		// ignored, and the cookie is domain-scoped (i.e. covers subdomains)
		// either way, so this still matches and deletes the legacy cookie in a
		// real browser. Compare against the normalized form.
		if c.Domain == strings.TrimPrefix(legacyImpersonateCookieDomain, ".") {
			clearedLegacy = true
		}
	}
	if !clearedHostOnly {
		t.Error("exit did not expire the host-only impersonation cookie")
	}
	if !clearedLegacy {
		t.Errorf("exit did not expire the legacy Domain=%s impersonation cookie — an admin who "+
			"started impersonating on the previous build cannot exit it", legacyImpersonateCookieDomain)
	}
}
