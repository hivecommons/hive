package dashboard

import (
	"os/exec"
	"testing"
)

// An optional panel failure must not strand the sidebar or live navbar on the
// previous status. Execute the real render entry point, not an ordering grep.
func TestStatusChromeUpdatesBeforePanelFailure10909(t *testing.T) {
	script := `
const assert = require('node:assert/strict');
const window = {scrollY: 0};
const document = {body: {style: {}}, getElementById() { return null; }};
let _statusInstance = '', _lastStatusSeq = 0, _statusSeqFloor = 0;
let currentAgentMetrics;
let _dropdownOpen = false, _configModalOpen = false;
let failAt = 'layout';
const calls = [];
function updateNavbarClockTimeZone() {}
function noteStatusPayloadTimestamp() {}
function tickNavbarClock() {}
function ocUpdateSidebarAgents() {
  calls.push(['sidebar', window._lastAgents.map(a => a.name), window._configuredAgents.map(a => a.name)]);
}
function renderAgentNavbarUpNext(agents) {
  assert.equal(window._lastStatus.agents, agents);
  calls.push(['navbar', agents.map(a => a.name)]);
}
function renderNavbarHealthBadge(el, data) { calls.push(['health', data.health]); }
function applyDashboardFeatureVisibility() { if (failAt === 'layout') throw new TypeError('null collection'); }
function updateInferenceModelsFromSSE() {}
function injectAgentStyles() {}
function renderGhRateLimits() {}
function renderGitHubAppBanner() {}
function renderRepoTargetBanner() {}
function renderIssuesDisabledBanner() {}
function refreshWelcomeAutoChecks() {}
function renderHubBanner() {}
function maybePlanningIntro() {}
function renderSystemAlerts() {}
function applyPinOverrides() {}
function renderAgents() { throw new TypeError('null collection'); }
` + jsFunc(t, indexHTML(t), "render") + `
for (const target of ['layout', 'agents']) {
  failAt = target;
  for (const seq of [1, 2]) {
    calls.length = 0;
    const data = {statusInstance: target, statusSeq: seq,
      agents: [{name: 'scanner-' + seq, state: 'running'}, {name: 'telemetry', state: 'stopped'}],
      configuredAgents: [{name: 'disabled-guide', enabled: false}], health: {ok: seq === 1}};
    assert.throws(() => render(data), /null collection/);
    assert.deepEqual(calls, [
      ['sidebar', ['scanner-' + seq, 'telemetry'], ['disabled-guide']],
      ['navbar', ['scanner-' + seq, 'telemetry']], ['health', data.health]
    ], target + ': navigation must use each fresh snapshot before a panel can fail');
    calls.length = 0;
    render({...data, statusSeq: seq - 1});
    assert.deepEqual(calls, [], 'stale snapshots must not repaint navigation');
  }
}
// Open settings/dropdowns may defer agent cards, not the live navigation.
_dropdownOpen = true;
_configModalOpen = true;
failAt = 'layout';
calls.length = 0;
assert.throws(() => render({agents: null, configuredAgents: null, health: null}), /null collection/);
assert.deepEqual(calls, [['sidebar', [], []], ['navbar', []], ['health', null]]);
`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("status chrome failure regression: %v\n%s", err, out)
	}
}
