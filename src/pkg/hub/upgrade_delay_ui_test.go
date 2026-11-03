package hub

import (
	"os/exec"
	"strings"
	"testing"
)

func TestUpgradeDelayWarningRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	parts := []string{
		"const assert = require('node:assert/strict');",
		"function esc(s) { return String(s || ''); }",
		"function escAttr(s) { return esc(s); }",
	}
	for _, name := range []string{"UPGRADE_PROGRESS_PILL_STATES", "UPGRADE_PROGRESS_LABELS", "UPGRADE_HISTORY_MAX_ROWS"} {
		parts = append(parts, extractHubJSVar(t, dashboardHTML, name)+";")
	}
	for _, name := range []string{"upgradeProgressLabel", "upgradeAgentsInTurnText", "formatUpgradeElapsed", "renderUpgradeHistory", "renderUpgradeProgress"} {
		parts = append(parts, jsFunc(t, name))
	}
	parts = append(parts, `
const hold = {trigger: 'window', outcome: 'delayed', reason: 'merge debounce', endedAt: '2026-10-30T13:04:00Z'};
const done = {outcome: 'done', target: 'abc1234', endedAt: '2026-10-30T13:08:00Z'};
const failed = {trigger: 'scheduled', outcome: 'missed', reason: 'ErrImagePull', endedAt: '2026-10-30T13:09:00Z'};
let html = renderUpgradeProgress({upgradeState: {state: 'delayed', reason: 'merge debounce'}, upgradeHistory: [hold]}, false);
assert.match(html, /is-delayed/);
assert.match(html, />Delayed</);
assert.match(html, /merge debounce/);
assert.doesNotMatch(html, /missed|history/);
assert.equal(renderUpgradeProgress({upgradeState: {state: 'done'}, upgradeHistory: [done]}, false), '');
assert.equal(renderUpgradeProgress({upgradeState: {state: 'done'}, upgradeHistory: [hold, done]}, false), '');
assert.equal(renderUpgradeProgress({upgradeState: {state: 'scheduled'}, upgradeHistory: [{...hold, outcome: 'missed'}, done]}, false), '');
assert.equal(renderUpgradeHistory({upgradeHistory: [done, {...failed, endedAt: '2026-10-30T13:00:00Z'}]}), '');
assert.match(renderUpgradeHistory({upgradeHistory: [done, failed]}), /1 missed/);
assert.match(renderUpgradeHistory({upgradeHistory: [failed]}), /ErrImagePull/);
`)
	if out, err := exec.Command(node, "-e", strings.Join(parts, "\n")).CombinedOutput(); err != nil {
		t.Fatalf("upgrade delay rendering failed: %v\n%s", err, out)
	}
}
