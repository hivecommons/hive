package hub

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/publicsuffix"
)

// hivecommons/hive#9785: signing in to a hosted hive looped between the hive
// and the hub's /login until the browser gave up (NS_ERROR_REDIRECT_LOOP).
// The hive's ingress sends the browser to /login only when the hub's
// auth-check answered 401 for the cookies the browser sent the HIVE, while
// /login bounced it straight back because the cookies the browser sent the HUB
// held a valid session. These tests drive both real handlers through a real
// cookie jar, so the browser's domain-matching decides which copy each side
// sees.

const (
	loopHubOrigin = "https://hive.hivecommons.dev"
	loopHiveID    = "hosted-projectbluefin-knuckle-gjvq"
	loopSpokeURL  = "https://hosted-projectbluefin-knuckle-gjvq.hive.hivecommons.dev/"
	// The URL the reporter's browser was stuck on, verbatim from the issue.
	loopReporterLoginURL = "https://hive.hivecommons.dev/login?redirect=https://hosted-projectbluefin-knuckle-gjvq.hive.hivecommons.dev/&rd=https://hosted-projectbluefin-knuckle-gjvq.hive.hivecommons.dev%2F"
	// Firefox's network.http.redirection-limit.
	browserRedirectLimit = 20
)

type loopBrowser struct {
	t   *testing.T
	hub *HubServer
	jar *cookiejar.Jar
	// hiveSendsCookies is false to model a hive whose auth-check never sees
	// a session the hub accepts, whatever the jar holds — the case where the
	// root cause is outside cookie scope.
	hiveSendsCookies bool
	loginURLs        []string
}

func newLoopBrowser(t *testing.T) *loopBrowser {
	t.Helper()
	t.Setenv("HIVE_HUB_PUBLIC_URL", loopHubOrigin)
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "kikaraage", Hives: map[string]string{loopHiveID: "read"}}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		t.Fatal(err)
	}
	return &loopBrowser{t: t, hub: newHandlerHub(), jar: jar, hiveSendsCookies: true}
}

func (b *loopBrowser) setCookie(rawURL string, c *http.Cookie) {
	u, _ := url.Parse(rawURL)
	b.jar.SetCookies(u, []*http.Cookie{c})
}

// visit navigates to start and follows redirects the way the browser and the
// hive's ingress-nginx do: the hive runs auth-url against the hub with the
// cookies the browser sends the hive, and on 401 redirects to auth-signin. It
// returns the page the browser settled on and how many redirects it took.
func (b *loopBrowser) visit(start string) (settled string, rec *httptest.ResponseRecorder, hops int) {
	b.t.Helper()
	next := start
	for hops = 0; hops <= browserRedirectLimit; hops++ {
		u, err := url.Parse(next)
		if err != nil {
			b.t.Fatalf("bad URL %q: %v", next, err)
		}
		switch {
		case u.Host == "hive.hivecommons.dev" && u.Path == "/login":
			b.loginURLs = append(b.loginURLs, next)
			req := httptest.NewRequest(http.MethodGet, next, nil)
			for _, c := range b.jar.Cookies(u) {
				req.AddCookie(c)
			}
			rec = httptest.NewRecorder()
			b.hub.handleLogin(rec, req)
			b.jar.SetCookies(u, rec.Result().Cookies())
			loc := rec.Header().Get("Location")
			if loc == "" {
				return next, rec, hops
			}
			ref, err := u.Parse(loc)
			if err != nil {
				b.t.Fatalf("bad Location %q: %v", loc, err)
			}
			next = ref.String()
		case u.Host == "hosted-projectbluefin-knuckle-gjvq.hive.hivecommons.dev":
			authReq := httptest.NewRequest(http.MethodGet,
				loopHubOrigin+"/api/saas/auth-check?hive="+loopHiveID+"&uri="+url.QueryEscape(u.RequestURI()), nil)
			if b.hiveSendsCookies {
				for _, c := range b.jar.Cookies(u) {
					authReq.AddCookie(c)
				}
			}
			rec = httptest.NewRecorder()
			b.hub.handleSaaSAuthCheck(rec, authReq)
			if rec.Code != http.StatusUnauthorized {
				return next, rec, hops
			}
			// auth-signin is $scheme://$http_host$request_uri; ingress-nginx
			// appends rd=$scheme://$http_host$escaped_request_uri.
			next = loopHubOrigin + "/login?redirect=" + next + "&rd=" + u.Scheme + "://" + u.Host + url.QueryEscape(u.RequestURI())
		default:
			b.t.Fatalf("browser left the hub and the hive: %q", next)
		}
	}
	b.t.Fatalf("redirect loop: still redirecting after %d hops; /login visits: %v", browserRedirectLimit, b.loginURLs)
	return "", nil, hops
}

// The two-copy jar: a valid host-only session on the hub host (which the hive
// never receives) next to a stale parent-scoped copy (which it does). The hub
// accepts the host-only copy, the hive rejects the stale one — the loop. The
// bounce re-issues the verified session at parent scope, replacing the stale
// copy, so the next hive request carries a session it accepts.
func TestLoginBounce_TwoCopyJarHealsInsteadOfLooping(t *testing.T) {
	b := newLoopBrowser(t)
	b.setCookie(loopHubOrigin+"/", &http.Cookie{Name: "hive_hub_user", Value: testAuthCookie("kikaraage").Value, Path: "/", Secure: true})
	b.setCookie(loopHubOrigin+"/", &http.Cookie{Name: "hive_hub_user", Value: "stale-session", Path: "/", Domain: "hivecommons.dev", Secure: true})

	settled, rec, hops := b.visit(loopSpokeURL)

	if settled != loopSpokeURL || rec.Code != http.StatusOK {
		t.Fatalf("settled on %q (auth-check %d), want the hive with its auth-check passing; /login visits: %v", settled, rec.Code, b.loginURLs)
	}
	if got := rec.Header().Get("X-Hive-User"); got != "kikaraage" {
		t.Errorf("auth-check X-Hive-User = %q, want kikaraage", got)
	}
	if len(b.loginURLs) != 1 || b.loginURLs[0] != loopReporterLoginURL {
		t.Errorf("/login visits = %v, want exactly the reporter's URL once", b.loginURLs)
	}
	if hops != 2 {
		t.Errorf("hops = %d, want 2 (hive → /login → hive)", hops)
	}
}

// When the hive rejects every session the hub sends it back with — a cause
// cookie scope cannot fix — /login must stop within its bounce budget and show
// a page, not redirect until the browser's limit.
func TestLoginBounce_PersistentRejectionStopsTheLoop(t *testing.T) {
	b := newLoopBrowser(t)
	b.hiveSendsCookies = false
	b.setCookie(loopHubOrigin+"/", &http.Cookie{Name: "hive_hub_user", Value: testAuthCookie("kikaraage").Value, Path: "/", Domain: "hivecommons.dev", Secure: true})

	settled, rec, _ := b.visit(loopSpokeURL)

	if !strings.HasPrefix(settled, loopHubOrigin+"/login") || rec.Code != http.StatusOK {
		t.Fatalf("settled on %q with %d, want the hub's /login stop page", settled, rec.Code)
	}
	if want := maxLoginBounces + 1; len(b.loginURLs) != want {
		t.Errorf("/login visited %d times, want %d (maxLoginBounces bounces, then stop)", len(b.loginURLs), want)
	}
	if !strings.Contains(rec.Body.String(), "hosted-projectbluefin-knuckle-gjvq.hive.hivecommons.dev") {
		t.Error("stop page does not name the host that rejected the session")
	}

	// The stop clears the budget, so "Try again" gets a full retry rather
	// than an immediate stop page.
	b.loginURLs = nil
	b.visit(loopSpokeURL)
	if want := maxLoginBounces + 1; len(b.loginURLs) != want {
		t.Errorf("retry after the stop visited /login %d times, want a fresh budget (%d)", len(b.loginURLs), want)
	}
}

// A protocol-relative target is another origin to the browser. The hub keeps
// //localhost (isTrustedRedirectTarget trusts localhost for dev), and a
// production .hivecommons.dev cookie never reaches localhost: bouncing there
// can only loop, and so can re-entering OAuth, so /login stops.
func TestLoginBounce_ProtocolRelativeTargetOutsideCookieScopeStops(t *testing.T) {
	b := newLoopBrowser(t)
	req := httptest.NewRequest(http.MethodGet, loopHubOrigin+"/login?redirect="+url.QueryEscape("//localhost:3000/"), nil)
	req.AddCookie(testAuthCookie("kikaraage"))
	rec := httptest.NewRecorder()
	b.hub.handleLogin(rec, req)

	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("/login redirected to %q, want the stop page: the session cookie cannot reach localhost", loc)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 stop page", rec.Code)
	}
}

func TestSessionCookieReaches(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", loopHubOrigin)
	for _, tc := range []struct {
		hubHost, target string
		want            bool
	}{
		{"hive.hivecommons.dev", "/dashboard", true},
		{"hive.hivecommons.dev", loopSpokeURL, true},
		{"hive.hivecommons.dev:443", loopSpokeURL, true},
		{"hive.hivecommons.dev", "//" + strings.TrimPrefix(loopSpokeURL, "https://"), true},
		{"hive.hivecommons.dev", "//localhost:3000/", false},
		{"hive.hivecommons.dev", "//127.0.0.1:3000/", false},
		{"hive.hivecommons.dev", "http://localhost:3000/", false},
		// A hub reached on a host outside its registrable domain mints a
		// host-only cookie, which reaches nothing but that host.
		{"hub-svc.hive-system.svc:8080", loopSpokeURL, false},
		{"localhost:8080", "http://localhost:3000/", true},
		{"localhost:8080", "http://127.0.0.1:3000/", false},
	} {
		if got := sessionCookieReaches(tc.hubHost, tc.target); got != tc.want {
			t.Errorf("sessionCookieReaches(%q, %q) = %v, want %v", tc.hubHost, tc.target, got, tc.want)
		}
	}
}
