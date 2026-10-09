package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestModalBackdropRulesBlurAndRespectReducedTransparency(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"--modal-scrim: rgba(0, 0, 0, 0.55)",
		"backdrop-filter: blur(6px)",
		"-webkit-backdrop-filter: blur(6px)",
		"prefers-reduced-transparency: reduce",
		"--modal-scrim-strong: rgba(0, 0, 0, 0.78)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("modal backdrop CSS missing %q", want)
		}
	}
}

func TestGlobalEscapeDismissesTopOverlay(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function hiveDismissTopOverlay()",
		"function hiveRememberModalOpen(el)",
		"function hiveModalOrder(el, fallback)",
		"document.addEventListener('keydown', function(e) {",
		"if (e.key !== 'Escape') return;",
		"if (e.isComposing) return;",
		"if (tag === 'select') return;",
		"candidates.sort(function(a, b) { return (b.rank - a.rank) || (b.order - a.order); });",
		"document.getElementById('feedback-overlay'), closeFeedbackModal",
		"document.getElementById('config-overlay'), closeConfigDialog",
		"document.querySelectorAll('.hive-dialog-overlay')",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("global Escape handler missing %q", want)
		}
	}
}

func TestGlobalEscapeDismissesModalsLIFO(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: modal Escape behavior was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
function makeClassList(values) { return { values: new Set(values || []), contains(v) { return this.values.has(v); }, add(v) { this.values.add(v); }, remove(v) { this.values.delete(v); } }; }
function modal(id) { return { id, dataset: {}, style: { display: 'flex' }, classList: makeClassList(['hive-dialog-overlay']), removed: false, hiveDialogClose() { this.removed = true; this.style.display = 'none'; closed.push(id); } }; }
const closed = [];
const elements = {};
let first = modal('first-modal');
let second = modal('second-modal');
elements[first.id] = first;
elements[second.id] = second;
global.getComputedStyle = el => ({ display: el && el.style ? el.style.display : 'none', zIndex: '20000' });
global.document = {
  activeElement: null,
  getElementById(id) { return elements[id] || null; },
  querySelectorAll(sel) { return sel === '.hive-dialog-overlay' ? [first, second].filter(el => el.style.display !== 'none') : []; },
  addEventListener() {}
};
function repoPillFilterActive() { return false; }
function clearRepoPillFilter() {}
function closeVersionMenu() {}
function hiveCloseGHUserMenu() {}
function dismissNPSSurvey() {}
function acmmCloseDialog() {}
function closeFeedbackModal() {}
function closeWelcomeDialog() {}
function closeConfigDialog() {}
function closeOverviewChartSettings() {}
function closeACMMDialog() {}
function closeNousConfig() {}
function cancelGHLogin() {}
let _hiveModalSequence = 0;
` + jsFunc(t, html, "hiveOverlayIsOpen") + jsFunc(t, html, "hiveOverlayRank") + jsFunc(t, html, "hiveRememberModalOpen") + jsFunc(t, html, "hiveModalOrder") + jsFunc(t, html, "hiveCloseGHUserMenu") + jsFunc(t, html, "hiveDismissTopOverlay") + `
hiveRememberModalOpen(first);
hiveRememberModalOpen(second);
assert.equal(hiveDismissTopOverlay(), true);
assert.deepEqual(closed, ['second-modal']);
assert.equal(first.style.display, 'flex');
assert.equal(second.style.display, 'none');
assert.equal(hiveDismissTopOverlay(), true);
assert.deepEqual(closed, ['second-modal', 'first-modal']);
assert.equal(hiveDismissTopOverlay(), false);
assert.deepEqual(closed, ['second-modal', 'first-modal']);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node modal Escape LIFO failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestModalEscapeRegistryCoversKnownDashboardOverlays(t *testing.T) {
	html := indexHTML(t)
	body := jsFunc(t, html, "hiveDismissTopOverlay")
	for _, want := range []string{
		"gh-auth-modal-overlay",
		"gh-setup-overlay",
		"hiveChatShortcuts",
		"hiveChatPanel",
		"config-overlay",
		"acmm-overlay",
		"welcome-overlay",
		"overview-chart-settings-overlay",
		"nous-config-overlay",
		"kb-modal-overlay",
		"plan-modal-overlay",
		"feedback-overlay",
		"acmm-dialog-overlay",
		"oc-drawer-backdrop",
		".hive-dialog-overlay",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("modal Escape registry no longer covers %q", want)
		}
	}
}

func TestRenderReleaseStatusCompactsSuccessfulUpgrade(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: release status rendering was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
let releaseEl = { hidden: true, innerHTML: '' };
global.document = { getElementById(id) { return id === 'release-status' ? releaseEl : null; } };
function escapeHtml(v) { return String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
function relativeAge() { return '10m ago'; }
function versionShortSHA(v) { return String(v || '').slice(0, 7); }
` + jsFunc(t, html, "renderReleaseStatus") + `
renderReleaseStatus({
  channel: { resolved: true, channel: 'candidate', selectorEnabled: true, selectorDetail: 'Choose stable, candidate, or edge. The hub records your intent.' },
  attempt: { state: 'succeeded', target: '0aca1bdabcdef', completedAt: '2026-10-03T11:33:17Z', detail: 'Last upgrade completed — the hive is running 0aca1bd.' }
});
assert.equal(releaseEl.hidden, false);
assert.match(releaseEl.innerHTML, /📡[\s\S]*Channel[\s\S]*release-channel-select/);
assert.match(releaseEl.innerHTML, /✅[\s\S]*Last upgrade[\s\S]*0aca1bd[\s\S]*10m ago/);
assert.match(releaseEl.innerHTML, /title="2026-10-03T11:33:17Z"/);
assert.doesNotMatch(releaseEl.innerHTML, /2026-10-03T11:33:17Z \(10m ago\)/);
assert.doesNotMatch(releaseEl.innerHTML, /the hive is running 0aca1bd/i);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node release status rendering failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestRenderReleaseStatusFallsBackToBuildBranch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: release status rendering was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
let releaseEl = { hidden: true, innerHTML: '' };
global.document = { getElementById(id) { return id === 'release-status' ? releaseEl : null; } };
function escapeHtml(v) { return String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
function relativeAge() { return '10m ago'; }
function versionShortSHA(v) { return String(v || '').slice(0, 7); }
` + jsFunc(t, html, "renderReleaseStatus") + `
renderReleaseStatus({
  channel: { resolved: false, branch: 'v6', selectorDetail: 'self-hosted Podman spoke' },
  attempt: { state: 'never' }
});
assert.match(releaseEl.innerHTML, /Tracking <strong>v6<\/strong>/);
assert.doesNotMatch(releaseEl.innerHTML, /Channel <strong>unknown/);
assert.doesNotMatch(releaseEl.innerHTML, /hub has not been reached/);
renderReleaseStatus({ channel: { resolved: false }, attempt: { state: 'never' }, hubReachable: false });
assert.match(releaseEl.innerHTML, /Channel <strong>unknown<\/strong>[\s\S]*image reference unavailable/);
assert.match(releaseEl.innerHTML, /hub has not been reached/);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node release status rendering failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
