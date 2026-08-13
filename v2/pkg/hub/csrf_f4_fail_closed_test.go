package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AUDIT F4 (replay half): isCSRFSafe used to fail OPEN.
//
// The old final line was `return strings.Contains(ct, "application/json")`, so a
// mutation with NO Origin and NO Referer was accepted as long as it claimed a
// JSON Content-Type. Content-Type is attacker-controlled and proves nothing
// about the caller, and a missing Origin/Referer is not evidence of a
// non-browser client — Referrer-Policy: no-referrer, privacy extensions and
// corporate proxies strip these routinely. The check now fails CLOSED.
//
// The tests below come in matched pairs. Each "must be refused" case is paired
// with a "must still succeed" positive control, because a check that simply
// returned false for every mutation would satisfy every regression here while
// breaking the entire dashboard.
//
// NOTE on the existing suite: TestIsCSRFSafe in hub_test.go carried a case
// asserting `{"POST with JSON content-type", "POST", "", "application/json",
// true}`. That was the vulnerability written down as an expectation, so it was
// INVERTED to `false` rather than deleted or relaxed.

const f4TrustedOrigin = "https://hive.kubestellar.io"

// TestF4HeaderlessJSONMutationIsRefused is the core regression.
func TestF4HeaderlessJSONMutationIsRefused(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/saas/hives", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			if isCSRFSafe(req) {
				t.Errorf("%s with a JSON Content-Type and no Origin/Referer was accepted; "+
					"CSRF must fail closed (F4)", method)
			}
		})
	}
}

// TestF4ContentTypeAloneNeverRescues: no Content-Type value is a substitute for
// origin information. Pinned across the shapes an attacker would reach for,
// including the ones a cross-site form POST can natively produce.
func TestF4ContentTypeAloneNeverRescues(t *testing.T) {
	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"text/plain",
		"application/x-www-form-urlencoded",
		"multipart/form-data",
		"",
	} {
		req := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if isCSRFSafe(req) {
			t.Errorf("POST with Content-Type %q and no Origin/Referer was accepted (F4)", ct)
		}
	}
}

// TestF4SameOriginMutationStillSucceeds is the POSITIVE CONTROL for the whole
// finding. A legitimate dashboard mutation — the exact request the real UI
// sends — must still pass. Without this, "reject every mutation" would satisfy
// every regression above.
func TestF4SameOriginMutationStillSucceeds(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method+"/Origin", func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/saas/hives", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", f4TrustedOrigin)
			if !isCSRFSafe(req) {
				t.Errorf("legitimate same-origin %s was REFUSED — the fix is too strict "+
					"and would break the dashboard", method)
			}
		})
		t.Run(method+"/Referer", func(t *testing.T) {
			// Origin absent but Referer present is a real browser shape.
			req := httptest.NewRequest(method, "/api/saas/hives", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Referer", f4TrustedOrigin+"/dashboard")
			if !isCSRFSafe(req) {
				t.Errorf("legitimate same-origin %s via Referer was REFUSED", method)
			}
		})
	}
	// Safe methods are untouched.
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		req := httptest.NewRequest(method, "/api/saas/my-hives", nil)
		if !isCSRFSafe(req) {
			t.Errorf("%s must remain safe", method)
		}
	}
}

// TestF4HostileOriginStillRefused: the exact-origin half of F4 (already fixed)
// must not regress. A sibling tenant receives the parent-domain session cookie
// and is therefore the most important hostile origin here.
func TestF4HostileOriginStillRefused(t *testing.T) {
	for _, origin := range []string{
		"https://evil.com",
		"https://hive.kubestellar.io.evil.com",
		"https://attacker-hive.hive.kubestellar.io", // sibling tenant
		"https://evil-hive.kubestellar.io",
		"://invalid",
		// NOT asserted here: "http://hive.kubestellar.io". isSameOriginAsHub
		// matches on HOST only and deliberately accepts localhost/127.0.0.1 for
		// local development, so it is scheme-agnostic by design. A scheme
		// downgrade on the real hub host is unreachable in practice (HSTS + the
		// Secure cookie), and tightening it is a separate change with its own
		// dev-flow blast radius — deliberately out of scope for F4 rather than
		// bundled in silently.
	} {
		req := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		if isCSRFSafe(req) {
			t.Errorf("hostile Origin %q was accepted", origin)
		}
	}
}

// TestF4BearerLaneIsTheExplicitNonBrowserPath: a non-browser API client
// authenticating with a bearer token and NO session cookie is allowed through,
// because it carries no ambient credential and so cannot be cross-site forged.
// This is the escape hatch that makes failing closed viable.
func TestF4BearerLaneIsTheExplicitNonBrowserPath(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer gho_sometoken")
	if !isCSRFSafe(req) {
		t.Error("a bearer-authenticated request with no session cookie must be allowed; " +
			"it carries no ambient credential and cannot be CSRF'd")
	}
}

// TestF4BearerDoesNotBypassWhenCookiePresent is the important half of the bearer
// lane, and the one that would be easy to get wrong. If merely ADDING a header
// let a cookie-bearing request skip the CSRF check, the fix would be worthless:
// the ambient credential is still in play and is still what the server would
// authenticate with.
func TestF4BearerDoesNotBypassWhenCookiePresent(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer gho_sometoken")
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "some-session-value"})
	if isCSRFSafe(req) {
		t.Error("a request carrying the ambient session cookie must prove its Origin " +
			"even when it also sends a bearer token (F4)")
	}

	// Even a junk cookie value counts — "browser sent a cookie" is the signal,
	// not "the cookie is valid". Otherwise the bypass condition is trivially
	// satisfiable by sending a garbage cookie.
	req2 := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
	req2.Header.Set("Authorization", "Bearer gho_sometoken")
	req2.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "not-a-valid-cookie"})
	if isCSRFSafe(req2) {
		t.Error("an invalid session cookie must not re-enable the bearer bypass")
	}

	// A non-bearer Authorization scheme is not the API lane either.
	req3 := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
	req3.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if isCSRFSafe(req3) {
		t.Error("a non-Bearer Authorization header must not pass the CSRF gate")
	}
}

// TestF4RequireAuthAndRequireAdminBothEnforce pins that the check is actually
// WIRED — a correct isCSRFSafe that no middleware calls protects nothing. An
// earlier audit found ~21 admin routes with no CSRF check at all; both
// middlewares must enforce it, and both must enforce it BEFORE resolving
// identity so a forged request never reaches the impersonation logic.
func TestF4RequireAuthAndRequireAdminBothEnforce(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")

	for _, tc := range []struct {
		name string
		wrap func(http.HandlerFunc) http.HandlerFunc
		user string
	}{
		{"requireAuth", s.requireAuth, "alice"},
		{"requireAdmin", s.requireAdmin, hubAdminUsername},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := tc.wrap(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})

			// Headerless JSON mutation with a VALID session cookie — the CSRF
			// scenario exactly. Must be refused, handler never reached.
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(testAuthCookie(tc.user))
			h(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("headerless mutation status = %d, want 403", rec.Code)
			}
			if called {
				t.Error("handler ran on a request that failed the CSRF check")
			}
			if !strings.Contains(rec.Body.String(), "CSRF") {
				t.Errorf("body = %q, want a CSRF refusal", rec.Body.String())
			}

			// POSITIVE CONTROL: same request, same user, WITH a legitimate
			// same-origin header — must reach the handler.
			called = false
			rec = httptest.NewRecorder()
			ok := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
			ok.Header.Set("Content-Type", "application/json")
			ok.Header.Set("Origin", f4TrustedOrigin)
			ok.AddCookie(testAuthCookie(tc.user))
			h(rec, ok)
			if rec.Code != http.StatusOK || !called {
				t.Errorf("legitimate same-origin mutation: status=%d called=%v; want 200 + handler run",
					rec.Code, called)
			}
		})
	}
}
