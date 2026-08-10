package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Admin read-only "View as user" impersonation.
//
// These tests exercise the security boundary directly (the cookie helpers,
// resolveIdentity, the write-block, and the enter/exit handlers) rather than
// the browser JS, because the enforcement lives entirely on the server. They
// reuse the shared test helpers from saas_handlers_coverage_test.go
// (testHubSecret, testAuthCookie, mkUser, newHandlerHub, reqWithUser,
// setPathValue, helperSetupTempDirs).

// impersonateCookie mints a signed impersonation cookie the same way the server
// does, so a test can present a genuine grant. It signs with the derived
// IMPERSONATE sub-key (C2 domain separation) — the same key the hub verifies
// impersonation cookies with. This key is HUB-ONLY: it is never derived for or
// injected into a spoke.
func impersonateCookie(admin, target string, now time.Time) *http.Cookie {
	return &http.Cookie{
		Name:  impersonateCookieName,
		Value: mintImpersonateCookieValue(deriveDomainKey(testHubSecret, infoImpersonateKey), admin, target, now),
	}
}

// TestImpersonateCookieRoundTrip verifies mint/verify agree and that tamper,
// wrong secret, and expiry all fail closed.
func TestImpersonateCookieRoundTrip(t *testing.T) {
	now := time.Now()
	val := mintImpersonateCookieValue(testHubSecret, "clubanderson", "alice", now)
	if val == "" {
		t.Fatal("mint returned empty for valid inputs")
	}
	g, ok := verifyImpersonateCookieValue(testHubSecret, val, now)
	if !ok || g.Admin != "clubanderson" || g.Target != "alice" {
		t.Fatalf("verify = %+v, ok=%v; want admin=clubanderson target=alice", g, ok)
	}

	// Wrong secret -> ignored.
	if _, ok := verifyImpersonateCookieValue("other-secret", val, now); ok {
		t.Error("verify accepted a cookie signed with a different secret")
	}
	// Tampered payload -> ignored.
	tampered := strings.Replace(val, "alice", "aliceX", 1)
	if _, ok := verifyImpersonateCookieValue(testHubSecret, tampered, now); ok {
		t.Error("verify accepted a tampered cookie")
	}
	// Expired -> ignored.
	if _, ok := verifyImpersonateCookieValue(testHubSecret, val, now.Add(impersonateTTL+time.Minute)); ok {
		t.Error("verify accepted an expired cookie")
	}
	// Empty target refused at mint.
	if mintImpersonateCookieValue(testHubSecret, "clubanderson", "", now) != "" {
		t.Error("mint accepted an empty target")
	}
}

// TestImpersonateStartAndIdentitySwitch: admin POSTs impersonate/{user}, gets a
// cookie, and a GET under that cookie resolves to the TARGET.
func TestImpersonateStartAndIdentitySwitch(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")

	// Admin starts impersonation of alice.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/admin/impersonate/alice", "", hubAdminUsername), "username", "alice")
	s.handleImpersonateStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var setCookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == impersonateCookieName {
			setCookie = c.Value
		}
	}
	if setCookie == "" {
		t.Fatal("start did not set the impersonation cookie")
	}

	// A GET carrying the admin session cookie + the impersonation cookie
	// resolves to alice.
	get := httptest.NewRequest(http.MethodGet, "/api/saas/my-hives", nil)
	get.AddCookie(testAuthCookie(hubAdminUsername))
	get.AddCookie(&http.Cookie{Name: impersonateCookieName, Value: setCookie})
	if eff := s.getAuthUser(get); eff != "alice" {
		t.Errorf("GET effective identity = %q, want alice", eff)
	}

	// resolveIdentity reports the real admin + impersonating=true on the GET.
	eff, real, imp := s.resolveIdentity(get)
	if eff != "alice" || real != hubAdminUsername || !imp {
		t.Errorf("resolveIdentity(GET) = (%q,%q,%v); want (alice,%s,true)", eff, real, imp, hubAdminUsername)
	}
}

// TestImpersonateStartUnknownTarget: 404 for a target with no user record.
func TestImpersonateStartUnknownTarget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/x", "", hubAdminUsername), "username", "ghost")
	s.handleImpersonateStart(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown target status = %d, want 404", rec.Code)
	}
}

// TestNonAdminCannotImpersonate: requireAdmin rejects a non-admin caller (403).
func TestNonAdminCannotImpersonate(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "bob")
	mkUser(t, "alice")

	h := s.requireAdmin(s.handleImpersonateStart)
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/admin/impersonate/alice", "", "bob"), "username", "alice")
	h(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin impersonate status = %d, want 403", rec.Code)
	}
}

// TestWriteBlockedDuringImpersonation: under an active grant, a write (POST/PUT/
// DELETE) is refused 403 by requireAuth/requireAdmin, while a GET still passes.
func TestWriteBlockedDuringImpersonation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	now := time.Now()

	called := false
	next := func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }

	// A POST through requireAuth under an active grant -> 403, next not called.
	guardedAuth := s.requireAuth(next)
	rec := httptest.NewRecorder()
	post := httptest.NewRequest(http.MethodPost, "/api/saas/hives", strings.NewReader("{}"))
	post.Header.Set("Content-Type", "application/json")
	post.AddCookie(testAuthCookie(hubAdminUsername))
	post.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	guardedAuth(rec, post)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST under impersonation status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("write handler ran despite impersonation write-block")
	}
	if !strings.Contains(rec.Body.String(), "read-only") {
		t.Errorf("403 body = %q, want a read-only message", rec.Body.String())
	}

	// A write through requireAdmin is likewise blocked.
	called = false
	guardedAdmin := s.requireAdmin(next)
	rec = httptest.NewRecorder()
	del := httptest.NewRequest(http.MethodDelete, "/api/saas/admin/users/alice", nil)
	del.AddCookie(testAuthCookie(hubAdminUsername))
	del.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	guardedAdmin(rec, del)
	if rec.Code != http.StatusForbidden {
		t.Errorf("admin DELETE under impersonation status = %d, want 403", rec.Code)
	}
	if called {
		t.Error("admin write handler ran despite impersonation write-block")
	}

	// A GET through requireAuth under the same grant still reaches the handler.
	called = false
	rec = httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/api/saas/my-hives", nil)
	get.AddCookie(testAuthCookie(hubAdminUsername))
	get.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	guardedAuth(rec, get)
	if rec.Code != http.StatusOK || !called {
		t.Errorf("GET under impersonation: status=%d called=%v; want 200 + handler called", rec.Code, called)
	}
}

// TestAdminReadSurfacesHiddenDuringImpersonation pins the UX-fidelity property of
// "View as": while impersonating, an admin-DATA GET (e.g. /api/saas/admin/users)
// must be refused so no admin surface leaks into the view — the impersonated
// dashboard shows exactly what the target user sees. The exit path stays callable.
func TestAdminReadSurfacesHiddenDuringImpersonation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	now := time.Now()

	called := false
	next := func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) }
	guardedAdmin := s.requireAdmin(next)

	// A GET admin-data route under an active grant -> 403, handler NOT reached, so
	// the client's 403 handling hides the admin Users section AND the Send Banner
	// controls. (Before the fix this returned 200 because requireAdmin honors the
	// real admin identity — leaking the full user list into the impersonated view.)
	rec := httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/api/saas/admin/users", nil)
	get.AddCookie(testAuthCookie(hubAdminUsername))
	get.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	guardedAdmin(rec, get)
	if rec.Code != http.StatusForbidden {
		t.Errorf("admin GET under impersonation status = %d, want 403 (admin surface must not leak)", rec.Code)
	}
	if called {
		t.Error("admin GET handler ran under impersonation — admin data leaked into the impersonated view")
	}

	// The SAME admin GET, NOT impersonating, still reaches the handler (200) — the
	// real admin retains full access when not viewing as someone.
	called = false
	rec = httptest.NewRecorder()
	get = httptest.NewRequest(http.MethodGet, "/api/saas/admin/users", nil)
	get.AddCookie(testAuthCookie(hubAdminUsername))
	guardedAdmin(rec, get)
	if rec.Code != http.StatusOK || !called {
		t.Errorf("admin GET without impersonation: status=%d called=%v; want 200 + handler called", rec.Code, called)
	}

	// The exit path stays reachable while impersonating (so the admin can always
	// get back out) even though it is behind requireAdmin.
	called = false
	rec = httptest.NewRecorder()
	guardedExit := s.requireAdmin(next)
	exitReq := httptest.NewRequest(http.MethodPost, impersonateExitPath, nil)
	// N6: requireAdmin now runs isCSRFSafe, so a POST must look like one the
	// dashboard actually sends — same-origin fetches carry Origin (and the JSON
	// content-type). Mirrors the requireAuth POST at :154-155.
	exitReq.Header.Set("Origin", "https://hive.kubestellar.io")
	exitReq.AddCookie(testAuthCookie(hubAdminUsername))
	exitReq.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	guardedExit(rec, exitReq)
	if rec.Code != http.StatusOK || !called {
		t.Errorf("exit path under impersonation: status=%d called=%v; want 200 + handler reached", rec.Code, called)
	}
}

// TestImpersonateExitClearsCookieAndStaysCallable: exit works WHILE impersonating
// (it is exempt from the write-block) and clears the cookie.
func TestImpersonateExitClearsCookieAndStaysCallable(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	now := time.Now()

	// Exit is registered behind requireAdmin AND is a POST — the write-block
	// must NOT fire for it. Drive it through the real middleware to prove that.
	guarded := s.requireAdmin(s.handleImpersonateExit)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, impersonateExitPath, nil)
	// N6: requireAdmin now enforces CSRF; send the Origin a real same-origin
	// dashboard fetch would carry.
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.AddCookie(testAuthCookie(hubAdminUsername))
	req.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	guarded(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("exit status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// The exit response must clear the cookie (MaxAge<0 / empty value).
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == impersonateCookieName && (c.MaxAge < 0 || c.Value == "") {
			cleared = true
		}
	}
	if !cleared {
		t.Error("exit did not clear the impersonation cookie")
	}

	// After exit (no impersonation cookie), identity is back to the admin.
	get := httptest.NewRequest(http.MethodGet, "/api/saas/my-hives", nil)
	get.AddCookie(testAuthCookie(hubAdminUsername))
	if eff := s.getAuthUser(get); eff != hubAdminUsername {
		t.Errorf("post-exit identity = %q, want %s", eff, hubAdminUsername)
	}
}

// TestForgedImpersonationCookieIgnored: a forged/tampered impersonation cookie
// is ignored — identity stays admin and no impersonation is reported.
func TestForgedImpersonationCookieIgnored(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")

	get := httptest.NewRequest(http.MethodGet, "/api/saas/my-hives", nil)
	get.AddCookie(testAuthCookie(hubAdminUsername))
	// A hand-crafted, unsigned value.
	get.AddCookie(&http.Cookie{Name: impersonateCookieName, Value: "clubanderson|alice|9999999999.forgedsig"})

	eff, real, imp := s.resolveIdentity(get)
	if imp {
		t.Error("forged impersonation cookie was honored")
	}
	if eff != hubAdminUsername || real != hubAdminUsername {
		t.Errorf("forged cookie shifted identity: eff=%q real=%q; want both %s", eff, real, hubAdminUsername)
	}
}

// TestImpersonationNoPrivilegeEscalation covers two escalation attempts:
//  1. A grant whose Admin field != the real signed user is ignored (a user
//     lifting the admin's cookie onto their own session).
//  2. A grant whose Admin field != hubAdminUsername is ignored (impersonation
//     can only ever be activated by the admin's real session).
func TestImpersonationNoPrivilegeEscalation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	mkUser(t, "bob")
	now := time.Now()

	// (1) bob presents a genuinely-signed grant that names the admin as actor,
	// on bob's OWN session. Because the real user is bob (not the grant.Admin
	// and not the admin), it must be ignored — bob stays bob, not elevated.
	get := httptest.NewRequest(http.MethodGet, "/api/saas/my-hives", nil)
	get.AddCookie(testAuthCookie("bob"))
	get.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	eff, real, imp := s.resolveIdentity(get)
	if imp || eff != "bob" || real != "bob" {
		t.Errorf("stolen admin cookie on bob's session: eff=%q real=%q imp=%v; want bob/bob/false", eff, real, imp)
	}

	// (2) A grant whose Admin field is a NON-admin, presented on that
	// non-admin's own session, must be ignored (only hubAdminUsername may
	// impersonate). Even though the signature is valid and admin==realUser here,
	// realUser != hubAdminUsername, so resolveIdentity refuses it.
	get2 := httptest.NewRequest(http.MethodGet, "/api/saas/my-hives", nil)
	get2.AddCookie(testAuthCookie("bob"))
	get2.AddCookie(impersonateCookie("bob", "alice", now))
	eff, real, imp = s.resolveIdentity(get2)
	if imp || eff != "bob" {
		t.Errorf("non-admin self-minted grant honored: eff=%q imp=%v; want bob/false", eff, imp)
	}
}

func TestImpersonationStatusReportsActiveGrant(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	now := time.Now()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/saas/admin/impersonation", nil)
	s.handleImpersonationStatus(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"impersonating":false`) {
		t.Fatalf("status without grant code=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/saas/admin/impersonation", nil)
	req.AddCookie(testAuthCookie(hubAdminUsername))
	req.AddCookie(impersonateCookie(hubAdminUsername, "alice", now))
	s.handleImpersonationStatus(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"impersonating":true`) || !strings.Contains(body, `"viewing_as":"alice"`) {
		t.Fatalf("status with grant code=%d body=%q", rec.Code, body)
	}
}
