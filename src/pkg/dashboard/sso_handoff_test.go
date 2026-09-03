package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
)

// testHubSecret is the shared hub secret used to mint/verify handoff tokens in
// these tests. It only has to be non-empty and stable within a test.
const testHubSecret = "test-hub-shared-secret"

// testHiveID is the hive the minted handoff tokens are scoped to.
const testHiveID = "hosted-test-hive-0001"

// newSSOServer builds a spoke Server wired for SSO handoff tests.
//
// hubProxied mirrors production shape: a hub-proxied spoke (hive-oke, nginx in
// front) has NO authorized_users allowlist, so IsDirectRouteAuthzEnabled() is
// false; a direct-route spoke (vllm-d, no proxy) carries the allowlist and is
// the authority on who may enter.
func newSSOServer(t *testing.T, hubProxied bool, authToken string, authorized ...string) *Server {
	t.Helper()
	s := newFullServer(t)
	s.authToken = authToken
	s.deps.Config.HiveID = testHiveID
	if hubProxied {
		s.deps.Config.Dashboard.AuthorizedUsers = nil
	} else {
		s.deps.Config.Dashboard.AuthorizedUsers = authorized
	}
	t.Setenv("HIVE_HUB_SECRET", testHubSecret)
	t.Setenv("HIVE_SSO_PUBLIC_KEY", "")
	t.Setenv("HIVE_SSO_PUBLIC_KEY_PREV", "")
	return s
}

// ssoGet drives handleSSO once and returns the recorder.
func ssoGet(t *testing.T, s *Server, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/sso?"+rawQuery, nil)
	// Production traffic arrives at the spoke over plain HTTP behind a
	// TLS-terminating nginx, which is what makes the cookie Secure.
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	s.handleSSO(w, r)
	return w
}

// sessionCookie returns the hive_session cookie the response set, or nil.
func sessionCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range (&http.Response{Header: w.Header()}).Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

// TestSSOHandoff_ValidTokenLandsOnRootWithSession is the happy path on BOTH the
// hub-proxied and the direct-route shapes: a valid handoff mints a session,
// sets the cookie, and redirects exactly once to the dashboard root — no
// further bounce.
func TestSSOHandoff_ValidTokenLandsOnRootWithSession(t *testing.T) {
	tests := []struct {
		name       string
		hubProxied bool
		authToken  string
		authorized []string
		wantRole   string
	}{
		{
			name:       "hub-proxied spoke with dashboard token",
			hubProxied: true,
			authToken:  "shared-secret-token",
			wantRole:   config.RoleOwner,
		},
		{
			// Regression: setSessionCookie used to be gated on authToken != "",
			// so a spoke provisioned without a dashboard token redirected to "/"
			// having set NO cookie — an infinite bounce.
			name:       "hub-proxied spoke with EMPTY dashboard token still sets a cookie",
			hubProxied: true,
			authToken:  "",
			wantRole:   config.RoleOwner,
		},
		{
			name:       "direct-route spoke, user on the allowlist",
			hubProxied: false,
			authToken:  "shared-secret-token",
			authorized: []string{"clubanderson"},
			wantRole:   config.RoleOwner,
		},
		{
			// The allowlist is authoritative for the role: the hub asked for
			// owner, the allowlist says read, read wins (no hub escalation).
			name:       "direct-route allowlist role overrides the token role",
			hubProxied: false,
			authToken:  "shared-secret-token",
			authorized: []string{"someoneelse", "clubanderson"},
			wantRole:   config.RoleRead,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSSOServer(t, tc.hubProxied, tc.authToken, tc.authorized...)
			tok := hub.MintSSOToken(hub.SSOSigningSeedFromMaster(testHubSecret), "clubanderson", config.RoleOwner, testHiveID, time.Now())
			if tok == "" {
				t.Fatal("failed to mint handoff token")
			}

			w := ssoGet(t, s, "token="+tok)

			if w.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303 (single redirect to root); body=%q", w.Code, w.Body.String())
			}
			loc := w.Header().Get("Location")
			if !strings.HasPrefix(loc, "/?") && loc != "/" {
				t.Fatalf("Location = %q, want the dashboard root", loc)
			}
			// The landing URL must NOT re-enter /sso — that is the loop.
			if strings.Contains(loc, "/sso") {
				t.Fatalf("Location = %q must not redirect back into the SSO handoff", loc)
			}

			c := sessionCookie(w)
			if c == nil {
				t.Fatal("no hive_session cookie set — the browser would bounce forever")
			}
			if c.Value == "" {
				t.Fatal("hive_session cookie set to an empty value")
			}
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if !c.Secure {
				t.Error("session cookie must be Secure behind TLS-terminating nginx")
			}
			if c.Path != "/" {
				t.Errorf("cookie Path = %q, want / so it is returned on the root request", c.Path)
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("cookie SameSite = %v, want Lax so it survives the cross-site handoff redirect", c.SameSite)
			}

			// The minted session must actually resolve to the right identity,
			// otherwise "/" rejects the very next request.
			sess := s.lookupSession(c.Value)
			if sess == nil {
				t.Fatal("session cookie does not resolve to a session")
			}
			if sess.Username != "clubanderson" {
				t.Errorf("session username = %q, want clubanderson", sess.Username)
			}
			if sess.Role != tc.wantRole {
				t.Errorf("session role = %q, want %q", sess.Role, tc.wantRole)
			}
		})
	}
}

// TestSSOHandoff_LandingRequestIsAuthenticated proves the loop is actually
// closed end to end: the cookie handleSSO set is accepted by authenticate() on
// the follow-up request to the landing URL, on both spoke shapes. If this fails,
// the browser bounces.
func TestSSOHandoff_LandingRequestIsAuthenticated(t *testing.T) {
	tests := []struct {
		name       string
		hubProxied bool
		authToken  string
		authorized []string
	}{
		{"hub-proxied", true, "shared-secret-token", nil},
		{"hub-proxied, no dashboard token", true, "", nil},
		{"direct-route", false, "shared-secret-token", []string{"clubanderson"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSSOServer(t, tc.hubProxied, tc.authToken, tc.authorized...)
			tok := hub.MintSSOToken(hub.SSOSigningSeedFromMaster(testHubSecret), "clubanderson", config.RoleOwner, testHiveID, time.Now())
			w := ssoGet(t, s, "token="+tok)
			c := sessionCookie(w)
			if c == nil {
				t.Fatal("handoff set no session cookie")
			}
			loc := w.Header().Get("Location")

			// Replay the browser's next request: GET the landing URL carrying
			// the cookie, through the real auth middleware.
			var sawUser string
			next := httptest.NewRequest("GET", loc, nil)
			next.AddCookie(&http.Cookie{Name: sessionCookieName, Value: c.Value})
			w2 := httptest.NewRecorder()
			s.authenticate(recordingHandler(&sawUser, nil)).ServeHTTP(w2, next)

			if w2.Code != http.StatusOK {
				t.Fatalf("landing request after handoff = %d, want 200 (a non-200 here IS the redirect loop)", w2.Code)
			}
			if sawUser != "clubanderson" {
				t.Errorf("landing request identity = %q, want clubanderson", sawUser)
			}
		})
	}
}

// TestSSOHandoff_FailuresTerminateWithoutRedirecting is the core anti-loop
// contract: every failure mode ends on an error page that explains itself. None
// of them may emit a redirect, because a redirect is what spins the browser.
func TestSSOHandoff_FailuresTerminateWithoutRedirecting(t *testing.T) {
	validTok := func() string {
		return hub.MintSSOToken(hub.SSOSigningSeedFromMaster(testHubSecret), "clubanderson", config.RoleOwner, testHiveID, time.Now())
	}

	tests := []struct {
		name       string
		hubProxied bool
		authorized []string
		hubSecret  string // "" means unset HIVE_HUB_SECRET
		query      func() string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing token",
			hubProxied: true,
			hubSecret:  testHubSecret,
			query:      func() string { return "" },
			wantStatus: http.StatusBadRequest,
			wantCode:   ssoErrMissingToken,
		},
		{
			name:       "garbage token",
			hubProxied: true,
			hubSecret:  testHubSecret,
			query:      func() string { return "token=not-a-real-token" },
			wantStatus: http.StatusUnauthorized,
			wantCode:   ssoErrBadToken,
		},
		{
			name:       "token signed with the wrong key",
			hubProxied: true,
			hubSecret:  testHubSecret,
			query: func() string {
				// A validly-signed token, but under a DIFFERENT hub master's seed:
				// its Ed25519 signature must not verify against this spoke's key.
				return "token=" + hub.MintSSOToken(hub.SSOSigningSeedFromMaster("a-different-master"), "clubanderson", config.RoleOwner, testHiveID, time.Now())
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   ssoErrBadToken,
		},
		{
			name:       "token scoped to a different hive",
			hubProxied: true,
			hubSecret:  testHubSecret,
			query: func() string {
				return "token=" + hub.MintSSOToken(hub.SSOSigningSeedFromMaster(testHubSecret), "clubanderson", config.RoleOwner, "some-other-hive", time.Now())
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   ssoErrBadToken,
		},
		{
			name:       "expired token",
			hubProxied: true,
			hubSecret:  testHubSecret,
			query: func() string {
				// Minted far enough in the past that any sane TTL has lapsed.
				const wellPastAnyTTL = -24 * time.Hour
				return "token=" + hub.MintSSOToken(hub.SSOSigningSeedFromMaster(testHubSecret), "clubanderson", config.RoleOwner, testHiveID, time.Now().Add(wellPastAnyTTL))
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   ssoErrBadToken,
		},
		{
			name:       "no hub secret configured on the spoke",
			hubProxied: true,
			hubSecret:  "",
			query:      func() string { return "token=anything" },
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   ssoErrNoSecret,
		},
		{
			name:       "direct-route: valid token, user NOT on the allowlist",
			hubProxied: false,
			authorized: []string{"someoneelse"},
			hubSecret:  testHubSecret,
			query:      func() string { return "token=" + validTok() },
			wantStatus: http.StatusForbidden,
			wantCode:   ssoErrNotAuthorized,
		},
		{
			name:       "hop counter at the limit trips the loop breaker",
			hubProxied: true,
			hubSecret:  testHubSecret,
			query: func() string {
				return "token=" + validTok() + "&" + ssoHopParam + "=" + strconv.Itoa(maxSSOHops)
			},
			wantStatus: http.StatusLoopDetected,
			wantCode:   ssoErrLoopDetected,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSSOServer(t, tc.hubProxied, "shared-secret-token", tc.authorized...)
			// newSSOServer sets the secret; override per case.
			t.Setenv("HIVE_HUB_SECRET", tc.hubSecret)

			w := ssoGet(t, s, tc.query())

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			// THE contract: a failed handoff must never redirect.
			if loc := w.Header().Get("Location"); loc != "" {
				t.Fatalf("failed handoff emitted a redirect to %q — this is the infinite-loop bug", loc)
			}
			if w.Code >= 300 && w.Code < 400 {
				t.Fatalf("failed handoff returned 3xx status %d — must terminate, not redirect", w.Code)
			}

			body := w.Body.String()
			if !strings.Contains(body, tc.wantCode) {
				t.Errorf("error page missing code %q; body=%q", tc.wantCode, body)
			}
			// The page must actually tell the user something actionable.
			if !strings.Contains(body, "Sign in with GitHub") {
				t.Error("error page must offer a direct sign-in escape hatch")
			}
			if !strings.Contains(body, "Back to the hub") {
				t.Error("error page must offer a way back to the hub")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("error page must not be cached, or the hive looks permanently broken")
			}
			// No session may be established on a failed handoff.
			if c := sessionCookie(w); c != nil && c.Value != "" && c.MaxAge >= 0 {
				t.Errorf("failed handoff established a session cookie %q", c.Value)
			}
		})
	}
}

// TestSSOHandoff_HopCounterAdvances checks the loop breaker actually counts:
// below the limit the handoff proceeds and increments, at the limit it stops.
func TestSSOHandoff_HopCounterAdvances(t *testing.T) {
	for hop := 0; hop < maxSSOHops; hop++ {
		s := newSSOServer(t, true, "shared-secret-token")
		tok := hub.MintSSOToken(hub.SSOSigningSeedFromMaster(testHubSecret), "clubanderson", config.RoleOwner, testHiveID, time.Now())
		w := ssoGet(t, s, "token="+tok+"&"+ssoHopParam+"="+strconv.Itoa(hop))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("hop=%d: status = %d, want 303 (still under the limit)", hop, w.Code)
		}
		want := ssoHopParam + "=" + strconv.Itoa(hop+1)
		if loc := w.Header().Get("Location"); !strings.Contains(loc, want) {
			t.Errorf("hop=%d: Location = %q, want it to carry %q", hop, loc, want)
		}
	}
}

// TestSSOErrorPage_NoLiteralBodyTag guards the snapshot builder, which does a
// naive indexOf("<body>") over the dashboard markup. A literal body tag
// introduced here corrupts the build.
func TestSSOErrorPage_NoLiteralBodyTag(t *testing.T) {
	if strings.Contains(ssoErrorPage, "<body>") {
		t.Error("ssoErrorPage must not contain a literal <body> tag — it corrupts the snapshot builder")
	}
}

// TestSSOErrorPage_EscapesInterpolatedText ensures the error page cannot be
// turned into an injection vector by any future caller.
func TestSSOErrorPage_EscapesInterpolatedText(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/sso", nil)
	writeSSOError(w, r, http.StatusForbidden, "CODE", `<script>alert(1)</script>`, "do a thing")
	if strings.Contains(w.Body.String(), "<script>alert(1)</script>") {
		t.Error("interpolated text must be HTML-escaped")
	}
}
