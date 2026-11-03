package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// #12512: the dashboard jumped every few seconds while scrolled below the
// Cost panel. renderCost ran on every SSE agent-status tick and rebuilt the
// panel with innerHTML, inserting the sub-tables expanded; applyCostCollapse
// then forced a layout (sparkline replay) before applyCostSubCollapse added
// `.collapsed`, so scroll anchoring moved the page by the sub-tables' height
// and rode the 200ms max-height transition back. Alongside, fetchAgentLogs
// polled /api/pane/<name> for disabled and stopped agents (no tmux window →
// 404 forever). These tests pin the in-place patching, the markup-time
// collapsed state, and the pane polling derived from the live agent list.

// TestPanePollingDerivedFromLiveAgentList asserts fetchAgentLogs only polls
// panes for agents the live list says can have one, and that a 404 parks the
// agent until its state changes.
func TestPanePollingDerivedFromLiveAgentList(t *testing.T) {
	html := indexHTML(t)
	fetch := jsFunc(t, html, "fetchAgentLogs")
	for _, snippet := range []string{
		"const agents = agentPaneCandidates(allAgents);",
		"const toFetch = current === 'all' ? agents : agents.filter(a => a.name === current);",
		"} else if (res.status === 404) {",
		"_paneAbsentState[a.name] = agentPaneStateKey(a);",
	} {
		if !strings.Contains(fetch, snippet) {
			t.Errorf("fetchAgentLogs is missing %q — pane polling must be derived from the live agent list and back off on 404", snippet)
		}
	}
	if strings.Contains(fetch, "agents.map(a => a.name)") {
		t.Error("fetchAgentLogs still polls every agent in window._lastAgents regardless of state")
	}
	candidates := jsFunc(t, html, "agentPaneCandidates")
	for _, snippet := range []string{
		"agentIsDisabled(a) || a.state === 'stopped'",
		"_paneAbsentState[a.name] !== agentPaneStateKey(a)) delete _paneAbsentState[a.name];",
	} {
		if !strings.Contains(candidates, snippet) {
			t.Errorf("agentPaneCandidates is missing %q", snippet)
		}
	}
}

// TestPaneCandidatesSkipAbsentAgents executes agentPaneCandidates under node:
// disabled and stopped agents are never candidates, a 404-parked agent stays
// parked while its state is unchanged, and is retried once the state moves.
func TestPaneCandidatesSkipAbsentAgents(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — agentPaneCandidates was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"function agentIsDisabled(a) { return !!a && a.enabled === false; }",
		"const _paneAbsentState = {};",
		jsFunc(t, html, "agentPaneStateKey"),
		jsFunc(t, html, "agentPaneCandidates"),
	}, "\n") + `
const fail = (m) => { console.log('FAIL ' + m); process.exitCode = 1; };
const names = (list) => agentPaneCandidates(list).map(a => a.name).join(',');
const live = [
  { name: 'scanner', enabled: true, state: 'running' },
  { name: 'quality', enabled: false, state: 'stopped' },
  { name: 'guide', enabled: false, state: 'stopped' },
  { name: 'halted', enabled: true, state: 'stopped' },
  { name: 'brainstorm', enabled: true, state: 'paused' },
];
if (names(live) !== 'scanner,brainstorm') fail('disabled/stopped agents were polled: ' + names(live));
// a 404 parks the agent in its current state …
_paneAbsentState['brainstorm'] = agentPaneStateKey(live[4]);
if (names(live) !== 'scanner') fail('404-parked agent was polled again: ' + names(live));
// … and a state change (paused → running) un-parks it
live[4] = { name: 'brainstorm', enabled: true, state: 'running' };
if (names(live) !== 'scanner,brainstorm') fail('agent not retried after its state changed: ' + names(live));
if (_paneAbsentState['brainstorm']) fail('stale absent mark was not cleared');
if (names([]) !== '' || names(null) !== '') fail('empty / null agent list must yield no candidates');
console.log('ok');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") || strings.Contains(string(out), "FAIL") {
		t.Fatalf("agentPaneCandidates behaviour regressed:\n%s\nerr=%v", out, err)
	}
}

// TestPeriodicSectionRendersPatchInPlace asserts the renders that run on a
// timer or SSE tick — cost (every agent-status tick), debug grid (10s) and
// audit table (30s) — patch their container through morphInnerHtml rather
// than rebuilding it with innerHTML, and that morphInnerHtml is the keyed
// morph from #12209 applied to a shallow clone of the container.
func TestPeriodicSectionRendersPatchInPlace(t *testing.T) {
	html := indexHTML(t)
	for _, tc := range []struct{ fn, container string }{
		{"renderCost", "el"},
		{"renderDebugSection", "grid"},
		{"renderAuditTable", "panel"},
	} {
		body := jsFunc(t, html, tc.fn)
		if strings.Contains(body, tc.container+".innerHTML =") {
			t.Errorf("%s still rebuilds %s with innerHTML — a periodic render must patch in place (#12512)", tc.fn, tc.container)
		}
		if !strings.Contains(body, "morphInnerHtml("+tc.container+",") {
			t.Errorf("%s does not call morphInnerHtml(%s, …)", tc.fn, tc.container)
		}
	}
	morph := jsFunc(t, html, "morphInnerHtml")
	for _, snippet := range []string{"const next = el.cloneNode(false);", "next.innerHTML = html;", "morphDomNode(el, next);"} {
		if !strings.Contains(morph, snippet) {
			t.Errorf("morphInnerHtml is missing %q", snippet)
		}
	}
}

// TestCostPanelRendersCollapsedStateIntoMarkup asserts the cost panel and its
// sub-tables are emitted already collapsed when persisted collapsed, and that
// the sub-table state is re-applied before applyCostCollapse forces a layout.
// An expanded-then-collapsed insert is exactly the transient height change
// that moved the page (#12512).
func TestCostPanelRendersCollapsedStateIntoMarkup(t *testing.T) {
	html := indexHTML(t)
	body := jsFunc(t, html, "renderCost")
	if !strings.Contains(body, "bodyId: 'cost-panel-body', collapsed: isCostCollapsed() }") {
		t.Error("renderCost does not pass collapsed: isCostCollapsed() to sectionCardShell")
	}
	subFirst := strings.Index(body, "applyCostSubCollapse();")
	panelAfter := strings.LastIndex(body, "applyCostCollapse();")
	if subFirst < 0 || panelAfter < 0 || subFirst > panelAfter {
		t.Error("renderCost must call applyCostSubCollapse() before applyCostCollapse() — the sparkline replay in applyCostCollapse forces a layout that must not see the sub-tables open")
	}
	for _, key := range []string{"sandbox", "session", "launch", "repo"} {
		for _, snippet := range []string{
			`<div class="cost-sub-body${costSubCollapsedCls('` + key + `')}" id="cost-sub-` + key + `-body">`,
			`aria-expanded="${costSubAriaExpanded('` + key + `')}" aria-controls="cost-sub-` + key + `-body"`,
		} {
			if !strings.Contains(html, snippet) {
				t.Errorf("cost sub-table %q does not render its collapsed state into the markup: missing %q", key, snippet)
			}
		}
	}
	shell := jsFunc(t, html, "sectionCardShell")
	if !strings.Contains(shell, "opts.collapsed ? 'collapsed' : ''") {
		t.Error("sectionCardShell ignores opts.collapsed")
	}
	header := jsFunc(t, html, "sectionCardHeader")
	if !strings.Contains(header, `aria-expanded="${opts.collapsed ? 'false' : 'true'}"`) {
		t.Error("sectionCardHeader does not reflect opts.collapsed in aria-expanded")
	}
}

// TestSectionCardShellEmitsCollapsedClasses executes sectionCardShell under
// node with collapsed: true and checks the body, card and chevron carry the
// collapsed class and the header is aria-expanded="false" — so a re-render
// of a collapsed panel never inserts an open body.
func TestSectionCardShellEmitsCollapsedClasses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sectionCardShell was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const esc = (s) => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/\"/g,'&quot;');",
		"const textOrDash = (v) => { const s = String(v == null ? '' : v).trim(); return s || '—'; };",
		"const visualSectionSummary = (id, text) => esc(text);",
		"const dashboardSectionHelpMark = () => '';",
		jsFunc(t, html, "sectionCardHeader"),
		jsFunc(t, html, "sectionCardShell"),
	}, "\n") + `
const fail = (m) => { console.log('FAIL ' + m); process.exitCode = 1; };
const open = sectionCardShell({ id: 'cost-panel', title: 'Cost', bodyId: 'cost-panel-body' }, 'x');
const shut = sectionCardShell({ id: 'cost-panel', title: 'Cost', bodyId: 'cost-panel-body', collapsed: true }, 'x');
if (!/class="dash-card"/.test(open) || !/class="dash-card-body section-body" id="cost-panel-body"/.test(open) || !/aria-expanded="true"/.test(open) || /collapsed/.test(open)) fail('expanded shell leaked a collapsed class: ' + open);
if (!/class="dash-card collapsed"/.test(shut)) fail('card missing collapsed class: ' + shut);
if (!/class="dash-card-body section-body collapsed" id="cost-panel-body"/.test(shut)) fail('body missing collapsed class: ' + shut);
if (!/class="section-chevron collapsed"/.test(shut)) fail('chevron missing collapsed class: ' + shut);
if (!/aria-expanded="false"/.test(shut)) fail('header not aria-expanded=false: ' + shut);
console.log('ok');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") || strings.Contains(string(out), "FAIL") {
		t.Fatalf("sectionCardShell collapsed markup regressed:\n%s\nerr=%v", out, err)
	}
}
