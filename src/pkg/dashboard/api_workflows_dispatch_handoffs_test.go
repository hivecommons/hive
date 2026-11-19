package dashboard

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A hand-off an agent declined or did not act on is shown where a profile
// nothing fills is: on the profile's line of the workflow row (GET
// /api/workflows) and on the item's tracker row (hivecommons/hive#18749).
func TestWorkflowDispatchHandoffsSurface(t *testing.T) {
	s := workflowTestServer(t, true)
	level := 5
	s.deps.Config.ACMMLevel = &level
	if got := s.workflowDispatchHandoffs(); got == nil || len(got) != 0 {
		t.Fatalf("unwired hand-offs = %v", got)
	}
	s.deps.WorkflowDispatchHandoffs = func() []WorkflowDispatchHandoff { return nil }
	if got := s.workflowDispatchHandoffs(); got == nil || len(got) != 0 {
		t.Fatalf("nil hand-offs = %v", got)
	}
	stalled := WorkflowDispatchHandoff{Ref: "repo1#7", Repo: "myorg/repo1", Number: 7, Profile: "techdocs-writer", State: "accepted", Agent: "scanner",
		Reason: "scanner declined the hand-off: my session is advisory", At: time.Now(), Attempts: 1, Retrying: true, Next: "the hive hands it to another agent that qualifies, if there is one"}
	s.deps.WorkflowDispatchHandoffs = func() []WorkflowDispatchHandoff {
		return []WorkflowDispatchHandoff{stalled, {Ref: "other#1", Repo: "myorg/other", Number: 1, Profile: "techdocs-writer"}, {Ref: "repo1#8", Repo: "myorg/repo1", Number: 8, Profile: "someone-else"}}
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows", techdocsModel(t), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1", "name": "cncf-techdocs", "version": "1"}, true); rec.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body)
	}
	rec := wfReq(s, http.MethodGet, "/api/workflows", nil, false)
	var out wfDispatchListing
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	d := wfDispatchOf(t, out, "techdocs-writer")
	if len(d.NotStarted) != 1 || d.NotStarted[0].Ref != "repo1#7" || !d.NotStarted[0].Retrying || !strings.Contains(d.NotStarted[0].Reason, "declined the hand-off") {
		t.Fatalf("the profile's not-started hand-offs = %+v", d.NotStarted)
	}
	if !strings.Contains(rec.Body.String(), `"not_started"`) || !strings.Contains(rec.Body.String(), `"candidates"`) {
		t.Fatalf("payload keys: %s", rec.Body)
	}

	// The tracker block of the repository carries it under the item number.
	w := &StageTrackerWorkflows{Repos: map[string]StageTrackerWorkflow{"myorg/repo1": {Name: "cncf-techdocs", Version: "1"}, "myorg/other2": {Name: "x", Version: "1"}}}
	attachWorkflowHandoffsToTracker(w, s.workflowDispatchHandoffs())
	h, ok := w.Repos["myorg/repo1"].Handoffs["7"]
	if !ok || h.Profile != "techdocs-writer" || h.State != "accepted" || h.Agent != "scanner" || !h.Retrying || h.Reason == "" {
		t.Fatalf("tracker hand-off = %+v %v", h, ok)
	}
	if len(w.Repos["myorg/other2"].Handoffs) != 0 {
		t.Fatalf("a hand-off landed on another repository: %+v", w.Repos["myorg/other2"])
	}
	attachWorkflowHandoffsToTracker(nil, s.workflowDispatchHandoffs())
	var nilServer *Server
	if got := nilServer.workflowDispatchStatuses(); got == nil || len(got) != 0 {
		t.Fatalf("nil server statuses = %v", got)
	}

	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"cncf/techdocs", "CNCF/TechDocs", true},
		{"cncf/techdocs", "techdocs", true},
		{"techdocs", "cncf/techdocs", true},
		{"cncf/techdocs", "other/techdocs", false},
		{"cncf/techdocs", "cncf/website", false},
		{"", "techdocs", false},
	} {
		if got := workflowRepoMatches(tc.a, tc.b); got != tc.want {
			t.Errorf("workflowRepoMatches(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// The shipped page: the picker marks the agents that may not do what the
// profile needs, the workflow row lists the hand-offs that were not
// started, and the Activity Monitor row wears a "not started" badge.
func TestWorkflowDispatchHandoffUI(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`wf.handoffs[String(Number(item.number) || 0)]`,
		`data-wf-stalled="1"`,
		`Array.isArray(d.candidates) ? d.candidates : []`,
		`Array.isArray(d.not_started) ? d.not_started : []`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing workflow hand-off wiring %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const assert = require('assert');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
` + jsConstLine(t, html, "WORKFLOW_DISPATCH_SOURCE_WORD") + "\n" +
		jsFunc(t, html, "workflowHandoffNote") + jsFunc(t, html, "workflowDispatchProfileView") + jsFunc(t, html, "renderRepoWorkflowProfileRows") +
		jsFunc(t, html, "stageTrackerWorkflowOf") + jsFunc(t, html, "stageTrackerWorkflowDispatchOf") +
		jsFunc(t, html, "stageTrackerWorkflowGapOf") + jsFunc(t, html, "stageTrackerWorkflowGapHtml") + `
const agents = ['quality', 'scanner'];
const drafter = { repo: 'cncf/techdocs', profile: 'techdocs-drafter', agent: 'quality', source: 'fallback', mode: 'ISSUES_AND_PRS',
  candidates: [
    { agent: 'quality', mode: 'ISSUES_AND_PRS', qualifies: true },
    { agent: 'scanner', mode: 'ADVISORY', qualifies: false, reason: 'may not open pull requests in cncf/techdocs (mode ADVISORY)' },
  ],
  not_started: [{ ref: 'cncf/techdocs#395', reason: 'scanner declined the hand-off: my session is advisory', retrying: true }] };

// The picker lists every agent, and marks the one that may not do the work.
const v = workflowDispatchProfileView(drafter, agents);
assert.deepStrictEqual(v.options.map(o => o.value), ['', 'quality', 'scanner']);
assert.strictEqual(v.options[1].label, 'quality');
assert.ok(!v.options[1].disabled);
assert.strictEqual(v.options[2].label, 'scanner (ADVISORY, cannot do this)');
assert.strictEqual(v.options[2].disabled, true);
assert.ok(v.options[2].title.indexOf('scanner may not open pull requests in cncf/techdocs') === 0, v.options[2].title);
assert.strictEqual(v.warning, '');
assert.deepStrictEqual(v.stalled, ['Hand-off of cncf/techdocs#395 not started: scanner declined the hand-off: my session is advisory. Decided at an unknown time in session unknown (before provenance was recorded). The hive hands it to another agent that may do the work, if there is one.']);

// A pin to an agent that may not do the work stays selectable (it is the
// current value) and the row warns.
const flagged = workflowDispatchProfileView({ profile: 'techdocs-drafter', pinned: 'scanner', problem: 'the pinned agent "scanner" may not open pull requests in cncf/techdocs (mode ADVISORY)', candidates: drafter.candidates }, agents);
assert.strictEqual(flagged.options[2].selected, true);
assert.strictEqual(flagged.options[2].disabled, false);
assert.ok(flagged.warning.indexOf('the pinned agent "scanner" may not open pull requests') >= 0, flagged.warning);
assert.deepStrictEqual(flagged.stalled, []);

const rows = renderRepoWorkflowProfileRows('techdocs', [drafter], agents, false);
assert.ok(rows.indexOf('<option value="scanner"  disabled title="scanner may not open pull requests in cncf/techdocs (mode ADVISORY)">scanner (ADVISORY, cannot do this)</option>') >= 0, rows);
assert.ok(rows.indexOf('data-wf-stalled="1" role="alert">Hand-off of cncf/techdocs#395 not started') >= 0, rows);

// Once the retry is used up the line says who acts next.
const waits = workflowDispatchProfileView({ profile: 'techdocs-drafter', agent: 'quality', not_started: [{ ref: 'cncf/techdocs#395', reason: 'quality ended its turn without opening the pull request', retrying: false }] }, agents);
assert.ok(waits.stalled[0].indexOf('It waits for an owner: use Send to techdocs-drafter') >= 0, waits.stalled[0]);

// The Activity Monitor row.
const td = { name: 'cncf-techdocs-assessment', version: '1',
  columns: [{ id: 'accepted', label: 'Accepted', states: ['accepted'], numbers: [20] }, { id: 'drafting', label: 'Drafting', states: ['drafting'], numbers: [70] }],
  dispatch: { '20': { profile: 'techdocs-drafter', state: 'accepted', kinds: ['issue'], agent: 'quality' } },
  handoffs: { '395': { profile: 'techdocs-drafter', state: 'accepted', agent: 'scanner', reason: 'scanner may not open pull requests in cncf/techdocs (mode ADVISORY)', retrying: true },
              '396': { profile: 'techdocs-drafter', state: 'accepted', agent: 'quality', reason: 'quality ended its turn without opening the pull request', retrying: false },
              '397': { profile: 'techdocs-drafter', state: 'some-earlier-state', agent: 'quality', reason: 'stale' } } };
const workflows = { default: { name: 'hive-default', version: '1', columns: [{ id: 'open', label: 'Open', states: ['open'], numbers: [] }] }, repos: { 'cncf/techdocs': td } };
const g = stageTrackerWorkflowGapOf({ repo: 'cncf/techdocs', kind: 'issue', number: 395, stage: 20 }, workflows);
assert.strictEqual(g.word, 'not started: techdocs-drafter');
assert.ok(g.tip.indexOf('Hand-off not started: scanner may not open pull requests in cncf/techdocs (mode ADVISORY). The hive hands it to another agent') === 0, g.tip);
const w = stageTrackerWorkflowGapOf({ repo: 'cncf/techdocs', kind: 'issue', number: 396, stage: 20 }, workflows);
assert.ok(w.tip.indexOf('It waits for an owner: use Send to techdocs-drafter') >= 0, w.tip);
const badge = stageTrackerWorkflowGapHtml({ repo: 'cncf/techdocs', kind: 'issue', number: 395, stage: 20 }, workflows);
assert.ok(badge.indexOf('class="ham-badge ham-badge-wf-gap"') >= 0 && badge.indexOf('not started: techdocs-drafter') >= 0, badge);
// A hand-off recorded for another state of the item, an item with none,
// and an item no longer in a dispatched state say nothing.
assert.strictEqual(stageTrackerWorkflowGapOf({ repo: 'cncf/techdocs', kind: 'issue', number: 397, stage: 20 }, workflows), null);
assert.strictEqual(stageTrackerWorkflowGapOf({ repo: 'cncf/techdocs', kind: 'issue', number: 398, stage: 20 }, workflows), null);
assert.strictEqual(stageTrackerWorkflowGapOf({ repo: 'cncf/techdocs', kind: 'issue', number: 395, stage: 70 }, workflows), null);
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node workflow hand-off fixture failed: %v\n%s", err, out)
	}
}
