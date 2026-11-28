package hub

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDashboardUpgradePauseSwitchesHiddenWhereInert pins #20253: the hub
// header's kill switches render only where they control something. The hub
// switch is hidden with release tracking off, the spoke switch on a
// self-hosted hub, a paused switch always shows so it can be resumed, and
// with neither switch the bordered cluster is dropped.
func TestDashboardUpgradePauseSwitchesHiddenWhereInert(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, "hubAutoCheck += upgradePauseClusterHTML();") {
		t.Fatal("hub header must render the kill switches through upgradePauseClusterHTML")
	}
	if n := strings.Count(html, "upgradePauseToggleHTML('hub'"); n != 1 {
		t.Errorf("found %d hub kill-switch renders, want 1 (inside upgradePauseClusterHTML)", n)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; upgradePauseClusterHTML behavior was NOT executed by this run")
	}
	script := `var _releaseTrackingDisabled = false, _hubSelfHosted = false;
var _upgradePause = {hub: {paused: false}, spokes: {paused: false}};
function upgradePauseToggleHTML(target) { return '[' + target + ']'; }
` + npsHubJSFunc(t, html, "upgradePauseClusterHTML") + `
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name + ': ' + upgradePauseClusterHTML()); } }
function set(tracking, selfHosted, hubPaused, spokesPaused) {
  _releaseTrackingDisabled = !tracking; _hubSelfHosted = selfHosted;
  _upgradePause = {hub: {paused: hubPaused}, spokes: {paused: spokesPaused}};
}
function has(t) { return upgradePauseClusterHTML().indexOf('[' + t + ']') >= 0; }
set(true, false, false, false);
check('default shows both', has('hub') && has('spokes'));
set(false, false, false, false);
check('tracking off hides hub only', !has('hub') && has('spokes'));
set(true, true, false, false);
check('self-hosted hides spokes only', has('hub') && !has('spokes'));
set(false, true, false, false);
check('tracking off + self-hosted drops the cluster', upgradePauseClusterHTML() === '');
set(false, true, true, false);
check('paused hub switch stays visible', has('hub') && !has('spokes'));
set(false, true, false, true);
check('paused spoke switch stays visible', !has('hub') && has('spokes'));
_upgradePause = null; _releaseTrackingDisabled = false; _hubSelfHosted = false;
check('missing pause state shows both', has('hub') && has('spokes'));
if (fails) process.exit(1);
`
	runDashboardJS(t, node, script)
}
