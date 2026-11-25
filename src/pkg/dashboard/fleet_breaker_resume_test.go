package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestFleetBreakerPausedChipOffersResume executes the shipped breaker chip
// render (#19778): a paused hive must always tell the viewer how to resume it.
// An owner's chip names the action in its visible label; a viewer who cannot
// release it gets a link to the hub My Hives page, and only for an http(s)
// URL; the link is hidden again once the hive runs.
func TestFleetBreakerPausedChipOffersResume(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the fleet breaker chip render was not executed")
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
function makeEl() {
  return {
    hidden: false, disabled: false, title: '', textContent: '', innerHTML: '',
    href: '', attrs: {}, style: {}, dataset: {},
    classList: { toggle() {} },
    setAttribute(n, v) { this.attrs[n] = v; },
    removeAttribute(n) { if (n === 'href') this.href = ''; delete this.attrs[n]; },
  };
}
const els = {};
for (const id of ['fleet-breaker-wrap', 'fleet-breaker-btn2', 'fleet-breaker-pill', 'fleet-breaker-dot', 'fleet-breaker-icon', 'fleet-breaker-hub-link']) els[id] = makeEl();
const document = { getElementById: id => els[id] || null };
const window = {};
function statusMeta(s) { return { status: s === 'paused' ? 'warn' : 'ok', label: s }; }
function dashboardRoleAtLeast(role, min) { return role === 'owner'; }
let _fleetBreakerEngaged = false;
`)
	script.WriteString(jsFunc(t, html, "fleetBreakerChipView"))
	script.WriteString("\n")
	script.WriteString(jsFunc(t, html, "_renderFleetBreaker"))
	script.WriteString(`
const btn = els['fleet-breaker-btn2'], label = els['fleet-breaker-pill'], link = els['fleet-breaker-hub-link'];

// Owner, paused: the chip is the resume control and says so.
window._hiveRole = 'owner';
window._myHivesURL = 'https://hub.example.com/dashboard';
_renderFleetBreaker(true, ['scanner', 'reviewer']);
assert.equal(btn.disabled, false, 'owner must be able to click the paused chip');
assert.match(label.textContent, /resume/, 'owner chip must name the resume action');
assert.match(btn.title, /2 agents/);
assert.equal(link.hidden, true, 'owner resumes from the chip; no hub link');

// Read-only viewer on a hub-linked hive: disabled chip plus a link to My Hives.
window._hiveRole = 'read';
_renderFleetBreaker(true, ['scanner']);
assert.equal(btn.disabled, true);
assert.equal(label.textContent, 'paused');
assert.equal(link.hidden, false, 'non-owner must be pointed at where resume lives');
assert.equal(link.href, 'https://hub.example.com/dashboard');
assert.match(btn.title, /My Hives/);

// Standalone hive or a non-http URL: no link, and the hover does not promise one.
for (const url of ['', 'javascript:alert(1)']) {
  window._myHivesURL = url;
  _renderFleetBreaker(true, ['scanner']);
  assert.equal(link.hidden, true, 'no hub link for ' + JSON.stringify(url));
  assert.equal(link.href, '');
  assert.doesNotMatch(btn.title, /My Hives/);
}

// Running again: the link goes away and the chip reads running.
window._myHivesURL = 'https://hub.example.com/dashboard';
_renderFleetBreaker(false, []);
assert.equal(link.hidden, true);
assert.equal(label.textContent, 'running');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("fleet breaker chip check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
