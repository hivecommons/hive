package dashboard

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestAgentConfigGetWorksForConfiguredDisabledAgent(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["guide"] = config.AgentConfig{
		Backend:      "claude",
		Model:        "sonnet",
		Enabled:      false,
		DisplayName:  "Guide",
		ModelOwner:   config.FieldOwnerPack,
		BackendOwner: config.FieldOwnerOperator,
	}

	rec := doGet(s, "/api/config/agent/guide")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for configured agent without a process: %s", rec.Code, rec.Body.String())
	}
	general := decodeJSON(t, rec)["general"].(map[string]any)
	if enabled, ok := general["enabled"].(bool); !ok || enabled {
		t.Fatalf("general.enabled = %#v, want false", general["enabled"])
	}
	if general["modelOwner"] != config.FieldOwnerPack || general["backendOwner"] != config.FieldOwnerOperator {
		t.Fatalf("ownership markers missing from config response: %#v", general)
	}
}

func TestConfiguredAgentsIncludesDisabledAgentsWithoutProcesses(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"running": {Enabled: true, SortOrder: 20},
		"guide":   {Enabled: false, SortOrder: 10, DisplayName: "Guide", ModelOwner: config.FieldOwnerPack},
	}}

	agents := buildConfiguredAgents(cfg)
	if len(agents) != 2 {
		t.Fatalf("configured agents = %d, want 2", len(agents))
	}
	if agents[0].Name != "guide" || agents[0].Enabled || agents[0].DisplayName != "Guide" {
		t.Fatalf("first configured agent = %#v, want disabled guide sorted first", agents[0])
	}
}

func TestDisabledAgentSidebarEnableWiring(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`window._configuredAgents = data.configuredAgents || [];`,
		`if (!configured.enabled && !runtimeNames.has(configured.name))`,
		`class="oc-nav-item disabled-agent"`,
		// Wiring, not prose: the button must open the agent config dialog on the
		// General tab. The visible label deliberately leads with a gear so the
		// row reads as configurable rather than as a one-click state toggle.
		`data-tab="General" title="Configure `,
		`>⚙ Enable…</button>`,
		`id="cfg-agent-enabled"`,
		`data-key="enabled"`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing disabled-agent enable wiring %q", snippet)
		}
	}
}

func TestRenderAgentsIncludesConfiguredDisabledCards(t *testing.T) {
	html := indexHTML(t)
	script := `
const window = { _lastAgents: [], _configuredAgents: [], _hiveRole: 'owner' };
global.window = window;
global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
var _sidebarLayout = null;
` + jsFunc(t, html, "agentIsDisabled") + "\n" +
		jsFunc(t, html, "_sidebarBuildGroups") + "\n" +
		jsFunc(t, html, "_sortAgentsBySidebar") + "\n" +
		jsFunc(t, html, "_sidebarAgents") + "\n" +
		jsFunc(t, html, "agentsSectionSummary") + `
const fixture = [
  { name: 'scanner', displayName: 'Scanner', enabled: true, sortOrder: 10, state: 'running', busy: 'idle', cli: 'copilot', model: 'gpt-5' },
  { name: 'outreach', displayName: 'Outreach', enabled: false, sortOrder: 20, state: 'stopped', busy: 'idle' }
];
const sorted = _sortAgentsBySidebar(fixture).sorted;
if (sorted.length !== 2) throw new Error('want two agents in card source, got ' + sorted.length);
if (sorted[0].name !== 'scanner' || sorted[1].name !== 'outreach') throw new Error('enabled agent must sort before disabled agent: ' + sorted.map(a => a.name).join(','));
const summary = agentsSectionSummary(sorted);
if (!summary.includes('1 on') || !summary.includes('1 disabled')) throw new Error('summary should count only enabled agent as on and append disabled count: ' + summary);
`
	cmd := exec.Command("node", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node renderAgents fixture failed: %v\n%s", err, out)
	}
	renderBody := jsFunc(t, html, "renderAgents")
	for _, snippet := range []string{
		`const isPoweredOff = isDisabled && a.state !== 'running';`,
		`class="agent-card ${cls}${compactCls}" data-agent="${esc(a.name)}"`,
		`<span class="status-badge disabled"`,
		`data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(configTarget)}" data-tab="General"`,
		"${agentStateText(a)}${pauseInfoIcon(a)}",
		"${isDisabled ? '' : `<div class=\"agent-actions\">",
	} {
		if !strings.Contains(renderBody, snippet) {
			t.Fatalf("renderAgents missing disabled-card snippet %q", snippet)
		}
	}
}

func TestDisabledAgentCadenceMatrixWiring(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`const agents = _sidebarAgents();`,
		`const statusAgentSet = new Set(agents.map(a => a.name));`,
		`agentDisabled ? '<span class="off-badge" data-status="neutral" title="Disabled in config — the governor does not schedule this agent">off</span>'`,
		"return `<td class=\"${colCls} cadence-disabled-cell\"",
		`<td class="power-col">${agentPowerSwitchHtml(agentData)}</td>`,
		`.gov-matrix tr.agent-disabled td.name-col { color: var(--muted); }`,
		`.gov-matrix td.cadence-disabled-cell, .gov-matrix td.continuous-cadence-note`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing disabled-agent cadence matrix wiring %q", snippet)
		}
	}
}

func TestContinuousAgentCadenceMatrixWiring(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`const isContinuous = val === 'continuous';`,
		`<span class="on-demand-badge" data-status="info">∞ continuous</span>`,
		`Re-kicks after Hive observes`,
		`role="radiogroup" aria-label="${esc(mode)} cadence mode"`,
		`∞ continuous`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing continuous cadence matrix wiring %q", snippet)
		}
	}
	for _, removed := range []string{
		`data-action="setAgentContinuous"`,
		`gov-continuous-switch`,
		`<th>Continuous</th>`,
		`.continuous-col`,
	} {
		if strings.Contains(html, removed) {
			t.Errorf("index.html still renders removed continuous table toggle %q", removed)
		}
	}
	if !strings.Contains(html, `cadenceKindButton(mode, kind, 'continuous', '∞ Continuous')`) || !strings.Contains(html, `∞ Continuous`) {
		t.Error("cadence editor must expose a segmented Continuous state")
	}
}

// TestConfiguredAgentsCarryResolvedMode pins the field the sidebar needs to
// show what a disabled agent WOULD do once enabled. A disabled agent has no
// runtime entry, so if buildConfiguredAgents omits the mode the badge silently
// renders nothing — dead markup that looks fine in review.
//
// Both branches matter: an explicit per-agent Mode must win, and an agent that
// sets none must still report the ACMM level default rather than empty.
func TestConfiguredAgentsCarryResolvedMode(t *testing.T) {
	level := 3
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"explicit": {Enabled: false, Mode: "ADVISORY"},
			"default":  {Enabled: false},
		},
	}

	got := map[string]FrontendConfiguredAgent{}
	for _, a := range buildConfiguredAgents(cfg) {
		got[a.Name] = a
	}

	if got["explicit"].Mode != "ADVISORY" {
		t.Errorf("explicit Mode override must win: got %q, want ADVISORY", got["explicit"].Mode)
	}
	if got["explicit"].ModeEmoji == "" {
		t.Error("explicit agent must carry a mode emoji for the sidebar badge")
	}
	if got["default"].Mode == "" {
		t.Error("agent with no Mode override must still report the ACMM level default, not empty")
	}
	if got["default"].ModeEmoji == "" {
		t.Error("level-default agent must carry a mode emoji too")
	}
}

// jsConstLine returns the single-line `const NAME = …;` declaration from
// index.html so a node harness runs against the shipped value.
func jsConstLine(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "const "+name+" = ")
	if start < 0 {
		t.Fatalf("index.html does not declare const %s", name)
	}
	end := strings.Index(html[start:], "\n")
	if end < 0 {
		t.Fatalf("unterminated const %s", name)
	}
	return html[start : start+end]
}

// TestAgentNowLineStates runs the agent card's "Now:" line renderer (#10934,
// part of #10925) over every state the line can show.
func TestAgentNowLineStates(t *testing.T) {
	html := indexHTML(t)
	script := strings.Join([]string{
		jsConstLine(t, html, "AGENT_NOW_UPDATE_MS"),
		jsConstLine(t, html, "AGENT_NOW_MISSED_UPDATES"),
		jsConstLine(t, html, "AGENT_NOW_HELP"),
		jsConstLine(t, html, "AGENT_NOW_PR_ACTIONS"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowHiveMs"),
		jsFunc(t, html, "agentNowIsOutdated"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "agentNowItem"),
		jsFunc(t, html, "agentNowItemHtml"),
		jsFunc(t, html, "agentNowReservations"),
		jsFunc(t, html, "agentNowLineHtml"),
	}, "\n") + `
const assert = require('assert');
const MIN = 60000;
const now = Date.parse('2026-01-01T12:00:00Z');
const iso = (ms) => new Date(ms).toISOString();
const repos = [
  { name: 'console', full: 'acme/console', actionableIssues: [{ number: 123, title: 'Fix the login redirect' }], openPrs: [{ number: 456, title: 'Redirect fix' }] },
  { name: 'docs', full: 'acme/docs' },
];
const claimsOn = (list) => ({ enabled: true, claims: list });
const ctx = (extra) => Object.assign({ hiveNowMs: now, outdated: false, repos, org: 'acme', ghBase: 'https://github.com', claims: { enabled: false, claims: [] } }, extra || {});
const st = (extra) => Object.assign({ isPoweredOff: false, isOff: false, isPaused: false, isStopped: false, isStarting: false, stateLabel: 'working' }, extra || {});
const text = (h) => h.replace(/<[^>]+>/g, '').replace(/&#39;/g, "'");
const agent = (extra) => Object.assign({ name: 'scanner', state: 'running', busy: 'working', lastKickAt: iso(now - 30 * MIN) }, extra || {});
const act = (repo, number, action, ago, current) => ({ repo, number, action, at: iso(now - ago), current });

// Working on an item, with the reservation on that item.
let out = agentNowLineHtml(agent({ lastAction: act('console', 123, 'agent_comment_created', 2 * MIN, true) }), st(),
  ctx({ claims: claimsOn([{ repo: 'acme/console', issue: 123, holder: 'scanner', holder_id: 'scanner', kind: 'agent', expires_at: iso(now + 90 * MIN) }]) }));
assert.ok(text(out).startsWith('Now: console#123 — Fix the login redirect · 2 min ago · reserved until '), out);
assert.ok(out.includes('href="https://github.com/acme/console/issues/123"'), out);
assert.ok(!/kick|turn|claim|busy/i.test(out), 'plain wording only: ' + out);

// A pull request links to the pull request; a missing title shows the number alone.
out = agentNowLineHtml(agent({ lastAction: act('acme/console', 456, 'agent_pr_created', 0, true) }), st(), ctx());
assert.strictEqual(text(out), 'Now: console#456 — Redirect fix · just now');
assert.ok(out.includes('/acme/console/pull/456'), out);
out = agentNowLineHtml(agent({ lastAction: act('console', 77, 'agent_comment_created', 5 * MIN, true) }), st(), ctx());
assert.strictEqual(text(out), 'Now: console#77 · 5 min ago');

// Working with nothing acted on this round; an earlier item is never "Now".
out = agentNowLineHtml(agent({ lastKickAt: iso(now - 12 * MIN), lastAction: act('console', 7, 'agent_comment_created', 60 * MIN, false) }), st(), ctx());
assert.strictEqual(text(out), 'Now: working — no issue or pull request yet · started 12 min ago');

// Idle, with and without a past item.
out = agentNowLineHtml(agent({ busy: 'idle', lastAction: act('console', 123, 'agent_comment_created', 40 * MIN, false) }), st({ stateLabel: 'idle' }), ctx());
assert.strictEqual(text(out), 'Idle · last: console#123, 40 min ago');
out = agentNowLineHtml(agent({ busy: 'idle' }), st({ stateLabel: 'idle' }), ctx());
assert.strictEqual(text(out), 'Idle');

// Other states show no item.
const cur = act('console', 123, 'agent_comment_created', MIN, true);
for (const [flags, a, want] of [
  [{ isPaused: true }, {}, 'Paused'],
  [{ isOff: true }, {}, 'Off'],
  [{ isPoweredOff: true }, { state: 'stopped' }, 'Off'],
  [{ isStopped: true }, { state: 'failed' }, "Stopped — see the agent's log"],
  [{ isStarting: true }, { state: 'starting' }, 'Not started yet'],
  [{}, { busy: 'idle', lastKickAt: '' }, 'Not started yet'],
]) {
  out = agentNowLineHtml(agent(Object.assign({ lastAction: cur }, a)), st(flags), ctx({ claims: claimsOn([{ repo: 'acme/console', issue: 1, holder: 'scanner', kind: 'agent', expires_at: iso(now + MIN) }]) }));
  assert.strictEqual(text(out), want);
}

// owner/repo when unwatched or when the short name is ambiguous.
out = agentNowLineHtml(agent({ lastAction: act('other/thing', 5, 'agent_comment_created', MIN, true) }), st(), ctx());
assert.strictEqual(text(out), 'Now: other/thing#5 · 1 min ago');
const twins = repos.concat([{ name: 'console', full: 'beta/console' }]);
out = agentNowLineHtml(agent({ lastAction: act('beta/console', 9, 'agent_comment_created', MIN, true) }), st(), ctx({ repos: twins }));
assert.ok(text(out).startsWith('Now: beta/console#9'), out);
assert.ok(out.includes('/beta/console/issues/9'), out);

// A viewer who is not shown any repository's issues sees no item or number.
out = agentNowLineHtml(agent({ busy: 'idle', lastAction: cur }), st({ stateLabel: 'idle' }), ctx({ repos: undefined }));
assert.strictEqual(text(out), 'Idle');

// Reservations: hidden when off or ended, "still reserved" for others, "and N more".
const three = claimsOn([3, 1, 2].map(n => ({ repo: 'acme/console', issue: n, holder: 'scanner', holder_id: 'scanner', kind: 'agent', expires_at: iso(now + n * MIN) }))
  .concat([{ repo: 'acme/console', issue: 4, holder: 'scanner', kind: 'agent', expires_at: iso(now - MIN) },
           { repo: 'acme/console', issue: 5, holder: 'reviewer', kind: 'agent', expires_at: iso(now + MIN) }]));
out = agentNowLineHtml(agent({ busy: 'idle' }), st({ stateLabel: 'idle' }), ctx({ claims: three }));
assert.ok(/^Idle · still reserved: console#1 until .+ and 2 more$/.test(text(out)), out);
out = agentNowLineHtml(agent({ busy: 'idle' }), st({ stateLabel: 'idle' }), ctx({ claims: Object.assign({}, three, { enabled: false }) }));
assert.strictEqual(text(out), 'Idle');

// Three missed updates: no live counting, say it may be out of date.
out = agentNowLineHtml(agent({ lastAction: cur }), st(), ctx({ outdated: true }));
assert.strictEqual(text(out), 'Now: console#123 — Fix the login redirect · information may be out of date');

// Hive clock: counts up from the payload timestamp, never negative.
const clock = { hiveMs: now, localMs: 5000 };
assert.strictEqual(agentNowHiveMs(clock, 65000), now + MIN);
assert.strictEqual(agentNowHiveMs(clock, 1000), now);
assert.strictEqual(agentNowAgo(-5 * MIN), 'just now');
assert.strictEqual(agentNowAgo(3 * 60 * MIN), '3 hours ago');
assert.ok(!agentNowIsOutdated(clock, 5000 + 3 * AGENT_NOW_UPDATE_MS));
assert.ok(agentNowIsOutdated(clock, 5000 + 4 * AGENT_NOW_UPDATE_MS));
`
	cmd := exec.Command("node", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node agent \"Now:\" line fixture failed: %v\n%s", err, out)
	}
}

// TestAgentNowLineWiring pins where the line renders: under the state line on
// the card (outside the details compact mode hides), in the detail panel and
// its quick-update path, and that the dead "Working on / PR open / merged"
// rows are gone.
func TestAgentNowLineWiring(t *testing.T) {
	html := indexHTML(t)
	renderBody := jsFunc(t, html, "renderAgents")
	state := strings.Index(renderBody, `${kickOutcomeBadgeHtml(a)}${agentSpendChip(a.name)}</div>`)
	now := strings.Index(renderBody, "${agentNowHtml(a)}")
	blockers := strings.Index(renderBody, "${agentBlockerLineHtml(a)}")
	details := strings.Index(renderBody, `<div class="agent-card-details"`)
	if state < 0 || now < state || blockers < now || details < blockers {
		t.Fatalf("renderAgents must render the Now line between the state line and the blockers line, before the card details (state=%d now=%d blockers=%d details=%d)", state, now, blockers, details)
	}
	detail := jsFunc(t, html, "ocRenderAgentDetail")
	if strings.Count(detail, "agentNowHtml(a)") != 2 {
		t.Fatalf("ocRenderAgentDetail must render the Now line in its full and quick-update paths")
	}
	for _, gone := range []string{"Working on (", "PR open (", "Merged today (", "no active fixes", "hive-merged-expanded"} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still contains the dead agent indicator row %q", gone)
		}
	}
	for _, want := range []string{"noteAgentNowUpdate(payload.timestamp);", "noteAgentNowUpdate(data && data.timestamp);"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing Now line clock wiring %q", want)
		}
	}
}
