package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// /login session short-circuit — a user who already carries a valid hub
// session and arrives at /login WITH a redirect target (the spoke auth-signin
// bounce) must be returned straight to that target. No provider picker, no
// second OAuth round-trip.
// ============================================================

// TestHandleLoginSignedInWithRedirectShortCircuits pins the fix: valid
// session + ?redirect= → 303 straight to the validated target.
func TestHandleLoginSignedInWithRedirectShortCircuits(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "octocat")

	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/login?redirect=%2Fdashboard", nil)
	req.AddCookie(testAuthCookie("octocat"))
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("signed-in /login?redirect= = %d, want %d (body=%q)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Fatalf("Location = %q, want /dashboard", loc)
	}
}

// TestHandleLoginSignedInWithoutRedirectKeepsLoginFlow pins that a bare
// /login visit (no redirect target) does NOT short-circuit even when signed
// in, so deliberately re-visiting login (e.g. to switch accounts) still works.
func TestHandleLoginSignedInWithoutRedirectKeepsLoginFlow(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "octocat")

	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/login", nil)
	req.AddCookie(testAuthCookie("octocat"))
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code == http.StatusSeeOther {
		t.Fatalf("bare /login short-circuited to %q — must keep the normal login flow", rec.Header().Get("Location"))
	}
}

// TestHandleLoginAnonymousWithRedirectKeepsLoginFlow pins that the
// short-circuit requires a VERIFIED session: an anonymous (or forged-cookie)
// request with a redirect target still goes through the login flow.
func TestHandleLoginAnonymousWithRedirectKeepsLoginFlow(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	for _, c := range []*http.Cookie{nil, {Name: "hive_hub_user", Value: "forged"}} {
		req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/login?redirect=%2Fdashboard", nil)
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		s.handleLogin(rec, req)
		if rec.Code == http.StatusSeeOther && rec.Header().Get("Location") == "/dashboard" {
			t.Fatalf("unauthenticated /login?redirect= short-circuited to the target — session verification is not being enforced")
		}
	}
}
