package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// These tests pin the client half of #7433 in the embedded static/index.html.
//
// An expired hub session makes the forward-auth gate in front of a hosted
// spoke answer 302 to the APEX login host. fetch()'s default redirect:'follow'
// chases that cross-origin, fails the CORS check, and rejects with an opaque
// "TypeError: Load failed" — so no `res.status === 401` branch in the file can
// ever run and the UI cannot tell "signed out" from "hive unreachable".
//
// The fix is redirect:'manual' on this hive's own /api calls: the redirect
// then arrives as an observable `opaqueredirect` response that the wrapper
// converts into the 401 the gate should have sent. The hive-api-xhr Ingress
// (#7405) makes the gate answer 401 directly, but it is provisioned per hive,
// so this guard is what covers hives that have not been re-provisioned.
//
// Browser behaviour itself stays manual-verify; what these tests guarantee is
// that the guard exists, that it is the only door to the network for /api, and
// that its scope does not swallow the one /api route whose redirect is real.

// fetchWrapperSource returns the source of the auth-token fetch wrapper IIFE,
// which is where the session-redirect guard lives. Scoping every assertion to
// this region keeps a page-wide strings.Contains from passing because some
// unrelated part of a 26k-line file happens to contain the same text.
func fetchWrapperSource(t *testing.T, html string) string {
	t.Helper()
	const start = "// Wrap fetch to inject auth token"
	i := strings.Index(html, start)
	if i < 0 {
		t.Fatalf("index.html has no auth-token fetch wrapper")
	}
	rest := html[i:]
	loc := regexp.MustCompile(`(?m)^    \}\)\(\);\n`).FindStringIndex(rest)
	if loc == nil {
		t.Fatal("could not find the end of the fetch wrapper IIFE")
	}
	return rest[:loc[1]]
}

// windowFetchBody returns the body of the `window.fetch = async function(...)`
// override inside the wrapper IIFE.
func windowFetchBody(t *testing.T, wrapper string) string {
	t.Helper()
	const decl = "window.fetch = async function(url, opts) {"
	i := strings.Index(wrapper, decl)
	if i < 0 {
		t.Fatalf("the fetch wrapper no longer declares %q", decl)
	}
	rest := wrapper[i:]
	loc := regexp.MustCompile(`(?m)^      \};\n`).FindStringIndex(rest)
	if loc == nil {
		t.Fatal("could not find the end of the window.fetch override")
	}
	return rest[:loc[1]]
}

// TestApiFetchUsesManualRedirect: the guard must ask for redirect:'manual'.
// Without it the browser follows the gate's cross-origin 302 and the call
// rejects before any status is observable (#7433).
func TestApiFetchUsesManualRedirect(t *testing.T) {
	wrapper := fetchWrapperSource(t, indexHTML(t))

	if !strings.Contains(wrapper, "async function _fetchWithSessionGuard(url, opts) {") {
		t.Fatal("the fetch wrapper has no _fetchWithSessionGuard; an expired session on /api still rejects as an opaque \"Load failed\" (#7433)")
	}
	if !strings.Contains(wrapper, "Object.assign({}, opts || {}, { redirect: 'manual' })") {
		t.Error("_fetchWithSessionGuard does not force redirect:'manual', so fetch() still follows the gate's cross-origin 302 and rejects with an unclassifiable TypeError (#7433)")
	}
}

// TestApiRedirectBecomesReadable401: an opaqueredirect is the expired session,
// and it must be handed to callers as the 401 the gate should have sent — so
// every `res.status === 401` branch in the dashboard stops being dead code.
func TestApiRedirectBecomesReadable401(t *testing.T) {
	wrapper := fetchWrapperSource(t, indexHTML(t))

	if !strings.Contains(wrapper, "if (res.type !== 'opaqueredirect') return res;") {
		t.Error("_fetchWithSessionGuard does not detect an opaqueredirect response, which is the only observable form the gate's 302 takes under redirect:'manual' (#7433)")
	}
	for _, want := range []string{
		"return new Response(SESSION_EXPIRED_BODY, {",
		"status: 401,",
		"'Content-Type': 'application/json'",
	} {
		if !strings.Contains(wrapper, want) {
			t.Errorf("_fetchWithSessionGuard does not synthesize a JSON 401 for an intercepted redirect; missing %q (#7433)", want)
		}
	}
	// The body is what a caller reads to offer "Sign in" instead of guessing
	// from a failed fetch, so its shape is part of the contract.
	if !strings.Contains(wrapper, "error: 'session_expired',") {
		t.Error("the synthesized 401 body does not carry error:'session_expired' (#7433)")
	}
	if !strings.Contains(wrapper, "sign_in: '/'") {
		t.Error("the synthesized 401 body does not carry a sign_in target, so the dashboard cannot offer a real Sign in action (#7433)")
	}
}

// TestGuardIsTheOnlyDoorForApiFetches: a guard that some call sites bypass
// fixes nothing. Every network call the wrapper makes — the primary request,
// the stale-token retry, and its own token/session probes — must go through it.
func TestGuardIsTheOnlyDoorForApiFetches(t *testing.T) {
	wrapper := fetchWrapperSource(t, indexHTML(t))
	body := windowFetchBody(t, wrapper)

	if !strings.Contains(body, "const res = await _fetchWithSessionGuard(url, opts);") {
		t.Error("the window.fetch override does not send its primary request through _fetchWithSessionGuard (#7433)")
	}
	if !strings.Contains(body, "return _fetchWithSessionGuard(url, retryOpts);") {
		t.Error("the stale-token retry bypasses _fetchWithSessionGuard, so a mutation retried after a token rotation still rejects opaquely (#7433)")
	}
	if strings.Contains(body, "_origFetch(") {
		t.Error("the window.fetch override still calls _origFetch directly; _fetchWithSessionGuard must be the only door to the network (#7433)")
	}

	// _ensureAuthToken's own probes are /api calls too, and their failures are
	// swallowed by a bare catch — an unguarded redirect there is silent.
	if strings.Contains(wrapper, "_origFetch('/api/") {
		t.Error("the wrapper probes /api with _origFetch, so an expired session there rejects and is swallowed by the surrounding catch (#7433)")
	}
	// _origFetch is still needed: it is the captured native fetch the guard
	// itself calls. Exactly one call site, inside the guard.
	if n := strings.Count(wrapper, "_origFetch(url, "); n != 2 {
		t.Errorf("_origFetch is invoked from %d site(s) with (url, opts); want exactly the 2 inside _fetchWithSessionGuard (guarded and unguarded branches)", n)
	}
}

// TestGuardScopeExcludesTheDeviceFlowLanding: GET /api/gh-user-auth/session is
// a real browser navigation whose own 302 back to "/" is its success path.
// Converting that into a 401 would break the self-hosted device-flow login.
func TestGuardScopeExcludesTheDeviceFlowLanding(t *testing.T) {
	wrapper := fetchWrapperSource(t, indexHTML(t))

	if !strings.Contains(wrapper, "const SESSION_REDIRECT_EXEMPT_PATHS = ['/api/gh-user-auth/session'];") {
		t.Error("the device-flow landing /api/gh-user-auth/session is not exempt from the redirect guard; its success redirect would be rewritten into a 401 (#7433)")
	}
	if !strings.Contains(wrapper, "return SESSION_REDIRECT_EXEMPT_PATHS.indexOf(u.pathname) === -1;") {
		t.Error("_isGuardedApiRequest does not consult SESSION_REDIRECT_EXEMPT_PATHS, so the exemption list is inert (#7433)")
	}
}

// TestGuardScopeIsThisHivesOwnApi: forcing redirect:'manual' on unrelated
// requests would break any third-party or page redirect the browser is
// supposed to follow, so the guard must be limited to same-origin /api.
func TestGuardScopeIsThisHivesOwnApi(t *testing.T) {
	wrapper := fetchWrapperSource(t, indexHTML(t))

	if !strings.Contains(wrapper, "function _isGuardedApiRequest(url) {") {
		t.Fatal("the fetch wrapper has no _isGuardedApiRequest scope check (#7433)")
	}
	if !strings.Contains(wrapper, "if (u.origin !== window.location.origin) return false;") {
		t.Error("_isGuardedApiRequest does not restrict the guard to same-origin requests, so a cross-origin redirect the caller expects to be followed would be rewritten into a 401 (#7433)")
	}
	if !strings.Contains(wrapper, "if (u.pathname !== '/api' && u.pathname.indexOf('/api/') !== 0) return false;") {
		t.Error("_isGuardedApiRequest does not restrict the guard to the /api prefix (#7433)")
	}
	if !strings.Contains(wrapper, "if (!_isGuardedApiRequest(url)) return _origFetch(url, opts);") {
		t.Error("_fetchWithSessionGuard does not fall through to an unmodified fetch for unguarded URLs (#7433)")
	}
}
