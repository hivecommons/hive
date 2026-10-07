package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAgentActivityRows runs the Agent activity row renderer (#10935, part of
// #10925) and checks that each row shows the same state word and "Now:" line
// as the agent's card, plus the time since its last action and every
// reservation it holds.
func TestAgentActivityRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — Agent activity rows were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const window = { _lastStatus: { governor: { mode: 'busy' } } };",
		jsConstLine(t, html, "AGENT_NOW_UPDATE_MS"),
		jsConstLine(t, html, "AGENT_NOW_MISSED_UPDATES"),
		jsConstLine(t, html, "AGENT_NOW_HELP"),
		jsConstLine(t, html, "AGENT_NOW_PR_ACTIONS"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentIsDisabled"),
		jsFunc(t, html, "agentHeldByMode"),
		jsFunc(t, html, "agentCadenceOffLabel"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "agentNowItem"),
		jsFunc(t, html, "agentNowItemHtml"),
		jsFunc(t, html, "agentNowReservations"),
		jsFunc(t, html, "agentNowLineHtml"),
		jsFunc(t, html, "agentNowStateOf"),
		jsFunc(t, html, "agentStateText"),
		jsFunc(t, html, "agentActivitySinceText"),
		jsFunc(t, html, "agentActivityReservationsHtml"),
		jsFunc(t, html, "agentActivityRowHtml"),
	}, "\n") + `
const assert = require('assert');
const MIN = 60000;
const now = Date.parse('2026-01-01T12:00:00Z');
const iso = (ms) => new Date(ms).toISOString();
const repos = [{ name: 'console', full: 'acme/console', actionableIssues: [{ number: 123, title: 'Fix the login redirect' }] }];
const claims = { enabled: true, claims: [
  { repo: 'acme/console', issue: 123, holder: 'scanner', holder_id: 'scanner', kind: 'agent', expires_at: iso(now + 90 * MIN) },
  { repo: 'acme/console', issue: 9, holder: 'scanner', holder_id: 'scanner', kind: 'agent', expires_at: iso(now + 30 * MIN) },
  { repo: 'acme/console', issue: 4, holder: 'scanner', holder_id: 'scanner', kind: 'agent', expires_at: iso(now - MIN) },
  { repo: 'acme/console', issue: 5, holder: 'reviewer', holder_id: 'reviewer', kind: 'agent', expires_at: iso(now + MIN) },
] };
const ctx = (extra) => Object.assign({ hiveNowMs: now, outdated: false, repos, org: 'acme', ghBase: 'https://github.com', claims }, extra || {});
const agent = (extra) => Object.assign({ name: 'scanner', state: 'running', busy: 'working', lastKickAt: iso(now - 30 * MIN) }, extra || {});
const act = (number, ago, current) => ({ repo: 'console', number, action: 'agent_comment_created', at: iso(now - ago), current });
const text = (h) => h.replace(/<[^>]+>/g, '').replace(/&#39;/g, "'").replace(/\s+/g, ' ').trim();
const cell = (row, cls) => {
  const m = new RegExp('<td class="' + cls + '">([\\s\\S]*?)</td>').exec(row);
  assert.ok(m, cls + ' missing in ' + row);
  return m[1];
};
const row = (a, c) => {
  const nowHtml = agentNowLineHtml(a, agentNowStateOf(a), c);
  const out = agentActivityRowHtml(a, nowHtml, c);
  assert.strictEqual(cell(out, 'aa-now'), nowHtml, 'row must show the card\'s Now line unchanged');
  return out;
};

// Working on an item: same Now line as the card, time since the action, every live reservation.
let a = agent({ lastAction: act(123, 2 * MIN, true) });
let out = row(a, ctx());
assert.strictEqual(text(cell(out, 'aa-name')), 'scanner');
assert.strictEqual(text(cell(out, 'aa-state')), 'working');
assert.ok(text(cell(out, 'aa-now')).startsWith('Now: console#123 — Fix the login redirect · 2 min ago'), out);
assert.strictEqual(text(cell(out, 'aa-since')), '2 min ago');
const reserved = text(cell(out, 'aa-reserved'));
assert.ok(/^console#9 until .+console#123 until .+$/.test(reserved), reserved);
assert.ok(!reserved.includes('#4') && !reserved.includes('#5'), 'ended and other holders\' reservations are not listed: ' + reserved);

// Working with no issue or pull request yet: time since it started.
out = row(agent({ lastKickAt: iso(now - 12 * MIN), lastAction: act(7, 60 * MIN, false) }), ctx({ claims: { enabled: false, claims: [] } }));
assert.strictEqual(text(cell(out, 'aa-since')), 'started 12 min ago');
assert.strictEqual(text(cell(out, 'aa-reserved')), '—');

// Other states show the same word as the card.
out = row(agent({ paused: true, lastAction: act(123, MIN, true) }), ctx());
assert.strictEqual(text(cell(out, 'aa-state')), 'paused');
assert.strictEqual(text(cell(out, 'aa-now')), 'Paused');
assert.strictEqual(agentStateText(agent({ enabled: false, state: 'stopped' })), 'powered off');
assert.strictEqual(agentStateText(agent({ onDemand: true })), 'on demand');
assert.strictEqual(agentStateText(agent({ awaitingCI: true })), 'Waiting on CI');
assert.strictEqual(agentStateText(agent({ offByCadence: true })), 'no cadence in busy');
assert.strictEqual(agentStateText(agent({ busy: 'idle' })), 'idle');

// A viewer not shown the item's repository sees no time for it either.
out = row(agent({ busy: 'idle', lastAction: act(123, 40 * MIN, false) }), ctx({ repos: undefined }));
assert.strictEqual(text(cell(out, 'aa-since')), '—');

// Out-of-date information stops counting.
out = row(agent({ lastAction: act(123, 2 * MIN, true) }), ctx({ outdated: true }));
assert.strictEqual(text(cell(out, 'aa-since')), '—');

// Names are escaped.
out = row(agent({ name: '<b>x</b>' }), ctx());
assert.ok(!out.includes('<b>x</b>'), out);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node Agent activity fixture failed: %v\n%s", err, out)
	}
}

// TestAgentActivitySectionWiring pins the section into the shared dashboard
// section machinery (collapse, reorder, hide, help) and to the same agent
// list and "Now:" refresh as the cards.
func TestAgentActivitySectionWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="agent-activity-section" data-dashboard-section="agent-activity-section"`,
		`data-action="toggleSection" data-arg0="agent-activity-section"`,
		`aria-label="Move Agent activity section"`,
		`data-section="agent-activity-section" data-action="ocNavigate"`,
		`'agent-activity-section': { title: '📍 Agent activity', summary: '0 working / 0 total' },`,
		`'agent-activity-section': { heading: 'Agent activity', help: `,
		`'agents-section', 'agent-activity-section', 'faq-section'`,
		`'agents-section','agent-activity-section','faq-section'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing Agent activity wiring %q", want)
		}
	}
	if !strings.Contains(jsFunc(t, html, "renderAgents"), "renderAgentActivity(agents);") {
		t.Error("renderAgents must feed the Agent activity section the same agent list as the cards")
	}
	if !strings.Contains(jsFunc(t, html, "refreshAgentNowLines"), "renderAgentActivity();") {
		t.Error("the Now line tick must also refresh the Agent activity rows")
	}
	render := jsFunc(t, html, "renderAgentActivity")
	for _, want := range []string{
		"refreshSectionCardShell('agent-activity-section')",
		"setSectionSummary('agent-activity-section'",
		"agentNowContext(Date.now())",
		"agentNowHtml(a)",
		"_sortAgentsBySidebar(",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderAgentActivity is missing %q", want)
		}
	}
}
