package hub

import (
	"regexp"
	"strings"
	"testing"
)

// The self-service registration form (#18060, #18145) lives entirely inside
// the dashboardHTML raw string, so there is no Go function to exercise. These
// tests guard the wiring: the status call that gates the feature, both CTA
// hooks, the modal and its one-time key panel, the three snippet tabs, and
// the owner's list with Rotate and Remove. They also pin the invariants the
// RFC cares about: no key placeholder in the asset, the key panel is emptied
// on close, and the flag-off path still links to the self-host guide.

// TestDashboardSelfServiceUXSymbols asserts every function the markup or the
// init path reaches for is defined and called. A typo is invisible to the Go
// compiler and only shows up as a dead click in the browser.
func TestDashboardSelfServiceUXSymbols(t *testing.T) {
	html := dashScript(t)
	for _, fn := range []string{
		"loadSelfServiceStatus",
		"selfServiceCtaClick",
		"openSelfServiceModal",
		"closeSelfServiceModal",
		"selfServiceClearKeyPanel",
		"submitSelfService",
		"selfServiceErrorMessage",
		"renderSelfServiceKey",
		"selfServiceCopy",
		"selfServiceTab",
		"loadSelfServiceHives",
		"renderSelfServiceList",
		"rotateSelfServiceHive",
		"removeSelfServiceHive",
	} {
		if !strings.Contains(html, "function "+fn+"(") &&
			!strings.Contains(html, "async function "+fn+"(") {
			t.Errorf("dashboardHTML is missing a definition for %s()", fn)
		}
		if strings.Count(html, fn+"(") < 2 {
			t.Errorf("dashboardHTML defines %s() but never calls it", fn)
		}
	}
}

// TestDashboardSelfServiceUXMarkup asserts the elements the JS reaches for by
// id are present: getElementById returning null is a silent no-op.
func TestDashboardSelfServiceUXMarkup(t *testing.T) {
	html := dashScript(t)
	for _, id := range []string{
		`id="btn-register-hive"`,
		`id="selfservice-modal"`,
		`id="ss-title"`,
		`id="ss-form-panel"`,
		`id="ss-prefix"`,
		`id="ss-name"`,
		`id="ss-org"`,
		`id="ss-repos"`,
		`id="ss-dash"`,
		`id="ss-error"`,
		`id="ss-cap"`,
		`id="ss-key-panel"`,
		`id="ss-close"`,
		`id="ss-submit"`,
		`id="selfservice-section"`,
		`id="selfservice-list"`,
		`id="selfservice-cap"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("dashboardHTML is missing element %s", id)
		}
	}
}

// TestDashboardSelfServiceUXAPIWiring asserts the asset talks to exactly the
// routes selfservice_api.go registers, and that the status call is what gates
// the feature.
func TestDashboardSelfServiceUXAPIWiring(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		`fetch('/api/hub/selfservice/status')`,
		`fetch('/api/hub/selfservice/hives')`,
		`fetch('/api/hub/selfservice/hives', {`,
		`method: 'POST'`,
		`/rotate', { method: 'POST' }`,
		`{ method: 'DELETE' }`,
		`loadSelfServiceStatus();`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML is missing %q", want)
		}
	}
	for _, status := range []string{"case 400:", "case 403:", "case 409:", "case 429:", "case 502:"} {
		if !strings.Contains(html, status) {
			t.Errorf("selfServiceErrorMessage does not map %s", status)
		}
	}
	// The 403 body carries the refused repo; the message must name it.
	if !strings.Contains(html, "data.repo") {
		t.Error("the 403 message does not use the repo the API names")
	}
}

// TestDashboardSelfServiceUXCTAs asserts both "Register your own hive" CTAs
// keep their link to the self-host guide (the flag-off path) and share the
// click hook that opens the form when the flag is on, and that the comments no
// longer claim the operator must issue HIVE_HUB_SECRET as the only path.
func TestDashboardSelfServiceUXCTAs(t *testing.T) {
	html := dashScript(t)
	const hook = `href="/get-started#self-host" onclick="return selfServiceCtaClick(event)"`
	if got := strings.Count(html, hook); got != 2 {
		t.Errorf("expected both Register-your-own-hive CTAs to carry %q, found %d", hook, got)
	}
	if !strings.Contains(html, `if (!_selfService.enabled) return true;`) {
		t.Error("selfServiceCtaClick must let the anchor navigate when the flag is off")
	}
	if strings.Contains(html, "requires a HIVE_HUB_SECRET issued by the hub operator") {
		t.Error("the CTA tooltip still says the operator must issue HIVE_HUB_SECRET")
	}
	if strings.Contains(html, "attaching a self-hosted hive is NOT self-service") {
		t.Error("the CTA comment still says attaching is not self-service; it should describe HIVE_HUB_SELFSERVICE_REGISTRATION")
	}
	if !strings.Contains(html, "HIVE_HUB_SELFSERVICE_REGISTRATION") {
		t.Error("the CTA comment does not name the flag that switches the behaviour")
	}
}

// TestDashboardSelfServiceUXKeyPanel pins the one-time key contract: the key
// is rendered only from the response, the panel is emptied on close, the
// three snippet tabs exist, and nothing in the asset looks like a key.
func TestDashboardSelfServiceUXKeyPanel(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		`key: data.key`,
		`'ss-out-key', r.key || ''`,
		`k.innerHTML = '';`,
		`selfServiceClearKeyPanel();`,
		`shown once and never again`,
		`{ key: 'docker_compose', label: 'Docker Compose' }`,
		`{ key: 'podman', label: 'Podman' }`,
		`{ key: 'kubernetes', label: 'Kubernetes' }`,
		`r.snippets[t.key]`,
		`esc(r.overlap)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML is missing %q", want)
		}
	}
	// closeSelfServiceModal must wipe the key panel, not just hide the modal.
	closeIdx := strings.Index(html, "function closeSelfServiceModal()")
	if closeIdx < 0 {
		t.Fatal("closeSelfServiceModal is missing")
	}
	closeBody := html[closeIdx:]
	if end := strings.Index(closeBody, "\n    }\n"); end > 0 {
		closeBody = closeBody[:end]
	}
	if !strings.Contains(closeBody, "selfServiceClearKeyPanel()") {
		t.Error("closeSelfServiceModal does not clear the key panel")
	}
	// The key must never be persisted or logged by the self-service code.
	ssStart := strings.Index(html, "Self-service (byo-) hive registration (#18060, #18145)")
	ssEnd := strings.Index(html, "async function init()")
	if ssStart < 0 || ssEnd < ssStart {
		t.Fatal("cannot locate the self-service script block")
	}
	block := html[ssStart:ssEnd]
	for _, forbidden := range []string{"localStorage", "sessionStorage", "console.log", "console.debug", "console.info"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("self-service script block must not use %s", forbidden)
		}
	}
	// No hard-coded key placeholder anywhere in the asset: the raw key comes
	// from the API response and nowhere else.
	placeholder := regexp.MustCompile(`(?i)(HIVE_HEARTBEAT_KEY\s*[:=]\s*["']?[A-Za-z0-9+/_-]{16,}|byo-[a-z0-9-]+-[a-z0-9]+\.[A-Za-z0-9_-]{20,})`)
	if m := placeholder.FindString(html); m != "" {
		t.Errorf("dashboardHTML contains what looks like a hard-coded key: %q", m)
	}
}

// TestDashboardSelfServiceUXEscaping asserts every server-provided string in
// the list and key panel is escaped before it reaches innerHTML.
func TestDashboardSelfServiceUXEscaping(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		`esc(h.id)`,
		`esc(h.org)`,
		`esc((h.repos || []).join(', ')`,
		`escAttr(h.dashboard_url)`,
		`jsArg(h.id)`,
		`esc(r.id || '')`,
		`esc(value)`,
		`esc(text)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML is missing escaped render %q", want)
		}
	}
}
