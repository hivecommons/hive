package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// These tests pin the client half of #7405 in the embedded static/index.html.
//
// On a hosted spoke an expired hub session made every fetch() reject
// ("TypeError: Load failed" — a cross-origin redirect to the IdP the browser
// would not follow), and loadConfigData answered that with `_configState.data
// = {}`: the agent's config dialog then rendered every field as 0/blank/off
// and armed Save to persist exactly those zeros. Three independent sites
// (config load, terminal assertion renewal, terminal handoff) each raised
// their own "…: Load failed" toast, none of which said "sign in".
//
// The rendered result is browser behaviour and stays manual-verify; what
// these tests guarantee is that the fallback is gone, the error state and its
// Save guard exist, and every one of the three sites goes through the one
// session-aware handler.

func TestConfigDialogNeverFabricatesConfig(t *testing.T) {
	html := indexHTML(t)

	fn := jsFunctionBody(t, html, "async function loadConfigData(type, agentName)")
	if strings.Contains(fn, "_configState.data = {}") {
		t.Error("loadConfigData still falls back to `_configState.data = {}` on failure — that is the fabricated all-zero config (#7405)")
	}
	if !strings.Contains(fn, "reportFetchFailure('Failed to load config', err)") {
		t.Error("loadConfigData does not route its failure through reportFetchFailure")
	}
	if !strings.Contains(fn, "_configState.loadError = expired ? 'session' : 'error'") {
		t.Error("loadConfigData does not record loadError, so nothing can tell a failed load from an empty one")
	}

	// The explicit error state, and the fact that no tab is rendered from the
	// placeholder once a load has failed.
	for _, snippet := range []string{
		"function renderConfigLoadError()",
		"Could not load configuration — not showing values",
		"if (_configState.loadError) { renderConfigLoadError(); return; }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	if n := strings.Count(html, "if (_configState.loadError) { renderConfigLoadError(); return; }"); n < 2 {
		t.Errorf("the load-error guard appears %d time(s); it must guard both the initial render (openConfigDialog) and every later tab switch (renderConfigTab)", n)
	}
}

func TestConfigDialogDisablesSaveWhenNothingLoaded(t *testing.T) {
	html := indexHTML(t)

	guard := jsFunctionBody(t, html, "function syncRepoSaveGuard()")
	if !strings.Contains(guard, "if (_configState.loadError) {") || !strings.Contains(guard, "why = CONFIG_NOT_LOADED_MESSAGE") {
		t.Error("syncRepoSaveGuard does not disable Save when the config never loaded (#7405)")
	}
	save := jsFunctionBody(t, html, "async function saveConfig()")
	if !strings.Contains(save, "if (_configState.loadError) {") || !strings.Contains(save, "showToast(CONFIG_NOT_LOADED_MESSAGE, 'error')") {
		t.Error("saveConfig does not refuse to PUT when the config never loaded; a racy click would persist the placeholder's zeros")
	}
}

// TestSessionExpiryHandlerIsShared: one handler, one probe, one message. Each
// of the three toast sites from the issue must call it, and the old per-site
// "…: Load failed" toasts must be gone.
func TestSessionExpiryHandlerIsShared(t *testing.T) {
	html := indexHTML(t)

	for _, snippet := range []string{
		"async function reportFetchFailure(context, err)",
		"function probeSession()",
		"const SESSION_EXPIRED_MESSAGE = 'Your session expired — sign in again';",
		"const SESSION_PROBE_PATH = '/api/gh-user-auth/status';",
		"function showSessionExpiredToast(signIn)",
		"btn.textContent = 'Sign in';",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	// The probe must not trust a status code that never arrives: a rejected
	// probe on a hosted host is the redirect, i.e. the session is gone.
	probe := jsFunctionBody(t, html, "function probeSession()")
	if !strings.Contains(probe, "if (r.status === 401 || r.status === 403) return { state: 'expired'") {
		t.Error("probeSession does not read an explicit 401/403 as an expired session")
	}
	if !strings.Contains(probe, "return isHostedHiveHost() ? { state: 'expired', signIn: 'reload' } : { state: 'unknown' };") {
		t.Error("probeSession does not treat a rejected probe on a hosted host as an expired session — the exact shape Safari reports as \"Load failed\"")
	}

	for _, site := range []struct{ fn, context string }{
		{"async function loadConfigData(type, agentName)", "Failed to load config"},
		{"async function renewTerminalAssertion()", "Could not refresh terminal access"},
		{"async function createTerminalHandoff()", "Could not prepare terminal access"},
	} {
		body := jsFunctionBody(t, html, site.fn)
		if !strings.Contains(body, "reportFetchFailure('"+site.context+"'") {
			t.Errorf("%s does not report its failure through reportFetchFailure(%q, …)", site.fn, site.context)
		}
		if strings.Contains(body, "showToast('"+site.context+": '") || strings.Contains(body, "showToast(`"+site.context+": ") {
			t.Errorf("%s still raises its own %q toast, so an expired session produces more than one message", site.fn, site.context)
		}
	}

	// Dedupe: a second expired-session report while the toast is up folds
	// into it rather than stacking.
	toast := jsFunctionBody(t, html, "function showSessionExpiredToast(signIn)")
	if !strings.Contains(toast, "if (_sessionExpiredToast && _sessionExpiredToast.parentNode) return _sessionExpiredToast;") {
		t.Error("showSessionExpiredToast does not deduplicate; three failing requests would still make three toasts")
	}
}

// jsFunctionBody returns the source of the named top-level function: from its
// declaration to the next line that is exactly four-space-indented `}`, which
// is how every top-level function in index.html closes.
func jsFunctionBody(t *testing.T, html, decl string) string {
	t.Helper()
	i := strings.Index(html, decl)
	if i < 0 {
		t.Fatalf("index.html has no %q", decl)
	}
	rest := html[i:]
	loc := regexp.MustCompile(`(?m)^    \}\n`).FindStringIndex(rest)
	if loc == nil {
		t.Fatalf("could not find the end of %q", decl)
	}
	return rest[:loc[1]]
}
