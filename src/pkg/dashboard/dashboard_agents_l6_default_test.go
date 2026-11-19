package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// At ACMM L6 the hive runs itself, so the Agents card starts collapsed until
// the operator picks a state. Level source: window._lastStatus.acmmLevel, the
// same field the header badge shows.
func TestAgentsSectionStartsCollapsedAtL6(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: the L6 default was NOT executed by this run")
	}
	html := indexHTML(t)
	script := dashboardCollapseDefaultsPreamble12014(t, html) + `
global.window = { _lastStatus: null };
dashboardSeedCollapseDefaults();
const A = 'agents-section';
// Level not known yet: today's default (expanded).
if (isSectionCollapsed(A)) throw new Error('unknown level must keep agents expanded');
// L5: today's default.
window._lastStatus = { acmmLevel: 5 };
if (isSectionCollapsed(A)) throw new Error('L5 must keep agents expanded');
// L6 and no choice: collapsed. Other critical cards are untouched.
window._lastStatus = { acmmLevel: 6 };
if (!isSectionCollapsed(A)) throw new Error('L6 with no choice must collapse agents');
for (const id of ['overview-section','governor','repos-section','review-queue-section']) if (isSectionCollapsed(id)) throw new Error('L6 must not collapse '+id);
// Level drops: normal default returns, the level default left no key behind.
window._lastStatus = { acmmLevel: 4 };
if (isSectionCollapsed(A)) throw new Error('dropping below L6 must restore the default');
if (localStorage.getItem(SECTION_LS_PREFIX+A) !== null) throw new Error('level default must not write a key');
// Operator expands at L6: remembered across a reload (fresh window state, same storage).
window._lastStatus = { acmmLevel: 6 };
setSectionCollapsed(A, false);
if (localStorage.getItem(SECTION_LS_PREFIX+A) !== '0') throw new Error('expand not remembered');
window._lastStatus = null; dashboardRememberAcmmLevel(6);
if (isSectionCollapsed(A)) throw new Error('expanded at L6 must stay expanded after reload');
window._lastStatus = { acmmLevel: 6 };
if (isSectionCollapsed(A)) throw new Error('late level must not collapse an operator-opened card');
// Expand made below L6 also survives a later rise to L6.
localStorage.removeItem(SECTION_LS_PREFIX+A);
window._lastStatus = { acmmLevel: 5 };
setSectionCollapsed(A, false);
window._lastStatus = { acmmLevel: 6 };
if (isSectionCollapsed(A)) throw new Error('expand at L5 must beat the L6 default');
// Operator collapses at L5: stays collapsed after the level drops.
localStorage.removeItem(SECTION_LS_PREFIX+A);
window._lastStatus = { acmmLevel: 5 };
setSectionCollapsed(A, true);
if (!isSectionCollapsed(A)) throw new Error('explicit collapse lost');
// Late level: the cached level makes the first paint right, status then confirms.
localStorage.removeItem(SECTION_LS_PREFIX+A);
window._lastStatus = null;
dashboardRememberAcmmLevel(6);
if (!isSectionCollapsed(A)) throw new Error('cached L6 must collapse before status arrives');
window._lastStatus = { acmmLevel: 6 };
if (!isSectionCollapsed(A)) throw new Error('status L6 must keep it collapsed (applied once, same answer)');
// Status with no level overrides a stale cache.
window._lastStatus = { acmmLevel: 0 };
if (isSectionCollapsed(A)) throw new Error('status level 0 must beat the cache');
// Legacy browsers (no collapse defaults) still get the level default.
localStorage.setItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY, DASHBOARD_COLLAPSE_DEFAULTS_LEGACY);
window._lastStatus = { acmmLevel: 6 };
if (!isSectionCollapsed(A)) throw new Error('legacy browser with no choice must collapse agents at L6');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node L6 default failed: %v\n%s", err, out)
	}
}

func TestAgentsSectionL6DefaultWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"dashboardRememberAcmmLevel(data.acmmLevel || 0);\n      COLLAPSIBLE_SECTION_IDS.forEach(applySectionCollapse);",
		"agentsAttentionBadgeHtml(agents) + (typeof agentsCollapsedTilesHtml",
		"need attention",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// No new timers for the level default.
	if strings.Contains(jsFunc(t, html, "dashboardAcmmLevel"), "setInterval") {
		t.Error("level default must not poll")
	}
}

// The collapsed summary must keep saying which agents need the owner.
func TestAgentsSummaryCountsAttention(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: the attention summary was NOT executed by this run")
	}
	html := indexHTML(t)
	script := `
function _agentNeedsLogin(a) { return a.needsLogin === true; }
function agentOutOfQuotaShown() { return false; }
` + jsFunc(t, html, "agentIsDisabled") + "\n" + jsFunc(t, html, "agentCollapsedAuthInfo") + "\n" +
		jsFunc(t, html, "agentsNeedingAttention") + "\n" + jsFunc(t, html, "agentsAttentionBadgeHtml") + "\n" +
		jsFunc(t, html, "agentsSectionSummary") + `
const ok = { name: 'a', enabled: true, state: 'running' };
const login = { name: 'b', enabled: true, state: 'running', needsLogin: true };
const down = { name: 'c', enabled: true, state: 'stopped' };
if (agentsSectionSummary([ok]).includes('attention')) throw new Error('healthy fleet must not claim attention');
const s = agentsSectionSummary([ok, login, down]);
if (!s.includes('2 need attention')) throw new Error('summary: ' + s);
if (!agentsAttentionBadgeHtml([ok, login]).includes('1 need attention')) throw new Error('badge');
if (agentsAttentionBadgeHtml([ok]) !== '') throw new Error('badge must be empty when nothing needs attention');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node attention summary failed: %v\n%s", err, out)
	}
}

// The sidebar Agents group owns its add controls: they collapse with it.
func TestSidebarAgentActionsInsideCollapsibleGroup(t *testing.T) {
	html := indexHTML(t)
	group := html[strings.Index(html, `id="oc-agent-nav-group"`):]
	group = group[:strings.Index(group, `<div class="oc-nav-label">Resources</div>`)]
	body := group[strings.Index(group, `id="oc-agent-tree-body"`):]
	for _, want := range []string{`class="oc-agent-actions"`, `data-action="openAddAgentDialog"`, `data-action="ocAddSidebarGroup"`, `id="oc-agent-tree"`} {
		if !strings.Contains(body, want) {
			t.Errorf("collapsible agent group body missing %q", want)
		}
	}
	if !strings.Contains(group, `class="oc-tree-children" id="oc-agent-tree-body"`) {
		t.Error("the group body must use the oc-tree-children class the toggle collapses")
	}
	for _, gone := range []string{"nous-sidebar-config", "nous-sb-mode", "nous-sb-phase"} {
		if strings.Contains(html, gone) {
			t.Errorf("the observe/phase lines were removed from the sidebar; %q is still present", gone)
		}
	}
}
