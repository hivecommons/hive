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
