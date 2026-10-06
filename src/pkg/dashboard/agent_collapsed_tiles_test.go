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
		"agent-tile-countdown",
		"agent-up-next",
		"up-next",
		`data-action="openAgentCollapsedTile"`,
		`data-agent-order-key`,
		`data-agent-next-kick-ms`,
		"function openAgentCollapsedTile(name)",
		"function formatKickCountdown(ms)",
		"function sortAgentsForCollapsedTiles(agents, nowMs)",
		"@media (prefers-reduced-motion: reduce) { .agent-collapsed-tile.active .agent-tile-dot { animation: none; } }",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("agents collapsed tile contract missing %q", want)
		}
	}
	for _, want := range []string{
		"agentsCollapsedTilesHtml(agents, Date.now())",
		"if ((sectionId === 'agents-section' || sectionId === 'repos-section') && inner) return inner;",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("agents collapsed summary visual helper missing %q", want)
		}
	}
}

func TestAgentsSidebarLinkAndNavbarUpNextContracts(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-section="agents-section" data-action="ocNavigate" data-arg0="agents-section"><span class="oc-nav-emoji">🤖</span><span class="oc-nav-text">Agents</span>`,
		`id="agent-navbar-upnext"`,
		`class="agent-navbar-upnext"`,
		`function renderAgentNavbarUpNext(agents, nowMs)`,
		`agentTileStates(agents, now).filter(item => agentNavbarHasSchedule(item.agent, now)).slice(0, 3)`,
		`function renderAgentNavbarTileDiff(wrap, list, now)`,
		`data-agent-key`,
		`function openAgentsUpNextPanel()`,
		`data-action="openAgentsUpNextPanel"`,
		`.agent-navbar-tile { flex: 0 1 clamp(136px, 11vw, 176px);`,
		`.agent-navbar-tile .agent-tile-name { grid-column: 1; grid-row: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }`,
		`renderAgentNavbarUpNext(agents, Date.now())`,
		`refreshAgentNavbarUpNextCountdowns(now);`,
		`@media (max-width: 1280px) { .agent-navbar-upnext { display: none !important; } }`,
		`@media (prefers-reduced-motion: reduce) { .agent-navbar-tile, .agent-navbar-tile.active .agent-tile-dot, .agent-navbar-tile.up-next-pulse { animation: none; transition: none; } }`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("agents sidebar/navbar up-next contract missing %q", want)
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
` + jsFunc(t, html, "formatKickCountdown") + `
` + jsFunc(t, html, "agentCollapsedNextKickMs") + `
` + jsFunc(t, html, "agentCollapsedNextLabel") + `
` + jsFunc(t, html, "agentTileIsRunning") + `
` + jsFunc(t, html, "agentCollapsedSortKey") + `
` + jsFunc(t, html, "agentTileStates") + `
` + jsFunc(t, html, "sortAgentsForCollapsedTiles") + `
const now = Date.UTC(2026, 9, 3, 11, 40, 0);
const agents = [
  { name: 'disabled', enabled: false, nextKick: '10/3 11:41 AM EDT', nextKickIn: '1m' },
  { name: 'continuous', continuous: true },
  { name: 'continuous-soon', continuous: true, nextKickIn: '2m' },
  { name: 'later', nextKick: '10/3 11:52 AM EDT', nextKickIn: '12m' },
  { name: 'active', busy: 'working', nextKick: '10/3 12:10 PM EDT', nextKickIn: '30m' },
  { name: 'sooner', nextKick: '10/3 11:43 AM EDT', nextKickIn: '3m' },
  { name: 'unknown' },
];
assert.deepEqual(sortAgentsForCollapsedTiles(agents, now).map(a => a.name), ['active', 'continuous-soon', 'sooner', 'later', 'continuous', 'unknown', 'disabled']);
assert.equal(formatKickCountdown(0), 'now');
assert.equal(formatKickCountdown(-1), 'now');
assert.equal(formatKickCountdown(90000), 'in 2m');
assert.equal(formatKickCountdown(null), '—');
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

func TestProjectsCollapsedSummaryRendersRepoTiles(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"repos-collapsed-tiles",
		"repo-collapsed-tile",
		"repo-collapsed-owner",
		"repo-collapsed-icons",
		"repo-collapsed-am",
		"function reposCollapsedTilesHtml(repos, data)",
		"function openRepoCollapsedTile(repo)",
		"function openRepoCollapsedMore()",
		"data-action=\"openRepoCollapsedTile\"",
		"data-arg0=\"${esc(parts.full)}\"",
		"data-action=\"openRepoCollapsedMore\"",
		"REPOS_COLLAPSED_TILE_LIMIT = 6",
		"repoPillFilterRepoMatches",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("projects collapsed repo tile contract missing %q", want)
		}
	}
	visual := jsFunctionBody(t, html, "function visualSectionSummary(sectionId, html, title)")
	for _, want := range []string{
		"reposCollapsedTilesHtml(repos, data)",
		"sectionId === 'repos-section'",
	} {
		if !strings.Contains(visual, want) {
			t.Fatalf("projects collapsed summary visual helper missing %q", want)
		}
	}
}
