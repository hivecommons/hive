package hub

import (
	"os/exec"
	"strings"
	"testing"
)

// TestHivePausedPillMarksBreakerPausedRow executes the shipped My Hives row
// helpers (#19778): a hive paused through the fleet breaker must say so in its
// row instead of looking healthy, offer resume to the owner or an admin, and
// stay quiet for a running hive or one whose agents were paused for any other
// reason (the agent-level pause has its own provenance in the Agents column).
func TestHivePausedPillMarksBreakerPausedRow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: hivePausedPill was not executed")
	}
	// esc, escAttr and jsArg are stand-ins: the shipped esc needs a DOM, and
	// jsFunc cannot cut out escAttr or jsArg (see avatarHarness).
	parts := []string{
		"const assert = require('node:assert/strict');",
		"function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }",
		"function escAttr(s) { return esc(s).replace(/\"/g, '&quot;').replace(/'/g, '&#39;'); }",
		`function jsArg(s) { return esc("'" + String(s == null ? '' : s).replace(/\\/g, '\\\\').replace(/'/g, "\\'") + "'"); }`,
		jsFunc(t, "agentPauseProvenance"),
		jsFunc(t, "hiveBreakerAgents"),
		jsFunc(t, "hivePausedPill"),
		`
const breakerPaused = { name: 'scanner', paused: true, pausedTrigger: 'fleet-breaker', pausedAt: new Date(Date.now() - 5 * 60000).toISOString() };
const paused = { id: 'hosted-abc', name: 'nate dubs tools', agents: [breakerPaused, Object.assign({}, breakerPaused, { name: 'reviewer' }), { name: 'guide', paused: false }] };

// Owner or admin: a button that resumes this hive.
const owner = hivePausedPill(paused, true);
assert.match(owner, /^<button type="button" class="hive-paused-pill"/);
assert.match(owner, /onclick="setHiveBreaker\('hosted-abc','nate dubs tools',false\)"/, 'resume must release the breaker, not engage it');
assert.match(owner, /resume<\/button>$/);
assert.match(owner, /holding 2 agents/);
assert.match(owner, /fleet-breaker/, 'hover carries the pause provenance');

// Anyone else: a label, no action, and a hover naming who can resume.
const viewer = hivePausedPill(paused, false);
assert.match(viewer, /^<span class="hive-paused-pill"/);
assert.doesNotMatch(viewer, /onclick/);
assert.match(viewer, /owner or a hub admin/);

// Running hive, no agents, or agents paused by something else: no pill.
assert.equal(hivePausedPill({ id: 'x', agents: [{ name: 'scanner', paused: false }] }, true), '');
assert.equal(hivePausedPill({ id: 'x' }, true), '');
assert.equal(hivePausedPill({ id: 'x', agents: [{ name: 'scanner', paused: true, pausedTrigger: 'dashboard-api' }] }, true), '');

// A hive id or name that tries to break out of the onclick string stays a
// string: every quote reaches the handler backslash-escaped.
const hostile = hivePausedPill({ id: "h'1", name: "x');alert(1);//", agents: [breakerPaused] }, true);
assert.ok(hostile.includes("setHiveBreaker('h\\'1','x\\');alert(1);//',false)"), hostile);
`,
	}
	if out, err := exec.Command(node, "-e", strings.Join(parts, "\n")).CombinedOutput(); err != nil {
		t.Fatalf("hivePausedPill check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
