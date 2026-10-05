package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAgentsCollapsedSummaryRendersPerAgentTiles(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"agents-collapsed-tiles",
		"agent-collapsed-tile",
		"agent-tile-name",
		"agent-tile-dot",
		"agent-tile-next",
		`data-action="openAgentCollapsedTile"`,
		`data-agent-order-key`,
		"function openAgentCollapsedTile(name)",
		"function sortAgentsForCollapsedTiles(agents, nowMs)",
		"@media (prefers-reduced-motion: reduce) { .agent-collapsed-tile.active .agent-tile-dot { animation: none; } }",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("agents collapsed tile contract missing %q", want)
		}
	}
	visual := jsFunctionBody(t, html, "function visualSectionSummary(sectionId, html, title)")
	for _, want := range []string{
		"agentsCollapsedTilesHtml(agents, Date.now())",
		"if (sectionId === 'agents-section' && inner) return inner;",
	} {
		if !strings.Contains(visual, want) {
			t.Fatalf("agents collapsed summary visual helper missing %q", want)
		}
	}
}

func TestAgentsCollapsedTileSortOrder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: agents collapsed tile sort helper was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
function agentIsDisabled(a) { return !!a && a.enabled === false; }
` + jsFunc(t, html, "agentCollapsedParseNextKick") + `
` + jsFunc(t, html, "agentCollapsedSortKey") + `
` + jsFunc(t, html, "sortAgentsForCollapsedTiles") + `
const now = Date.UTC(2026, 9, 3, 11, 40, 0);
const agents = [
  { name: 'disabled', enabled: false, nextKick: '10/3 11:41 AM EDT', nextKickIn: '1m' },
  { name: 'continuous', continuous: true },
  { name: 'later', nextKick: '10/3 11:52 AM EDT', nextKickIn: '12m' },
  { name: 'active', busy: 'working', nextKick: '10/3 12:10 PM EDT', nextKickIn: '30m' },
  { name: 'sooner', nextKick: '10/3 11:43 AM EDT', nextKickIn: '3m' },
  { name: 'unknown' },
];
assert.deepEqual(sortAgentsForCollapsedTiles(agents, now).map(a => a.name), ['active', 'sooner', 'later', 'continuous', 'unknown', 'disabled']);
assert.equal(agentCollapsedParseNextKick('due now', now), now);
assert.equal(agentCollapsedParseNextKick('continuous', now), null);
assert.equal(agentCollapsedParseNextKick('in 3m', now), now + 180000);
assert.equal(agentCollapsedParseNextKick('3m', now), now + 180000);
assert.equal(agentCollapsedParseNextKick('1h 5m', now), now + 3900000);
assert.equal(new Date(agentCollapsedParseNextKick('10/3 11:43 AM EDT', now)).getUTCFullYear(), 2026);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("agents collapsed tile sort helper failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestAgentsCollapsedTileCountdownAndUpNext(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"agent-tile-upnext",
		"agent-tile-cd",
		"function agentCollapsedCountdownText(a, nextMs, nowMs)",
		"function agentCollapsedMaybeTickCountdowns(nowMs)",
		"agentCollapsedTileHtml(a, nowMs, i === 0)",
		"agentCollapsedMaybeTickCountdowns(now.getTime());",
		"'logged out'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("agents collapsed tile countdown contract missing %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: countdown helper was not executed")
	}
	script := `const assert = require('node:assert/strict');
function agentIsDisabled(a) { return !!a && a.enabled === false; }
` + jsFunc(t, html, "agentCollapsedCountdownText") + `
const now = 1000000;
assert.equal(agentCollapsedCountdownText({}, now + 240000, now), 'in 4m');
assert.equal(agentCollapsedCountdownText({}, now + 10000, now), 'now');
assert.equal(agentCollapsedCountdownText({ busy: 'working' }, now + 900000, now), 'now');
assert.equal(agentCollapsedCountdownText({}, null, now), '—');
assert.equal(agentCollapsedCountdownText({ enabled: false }, now + 240000, now), '—');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("countdown helper failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
