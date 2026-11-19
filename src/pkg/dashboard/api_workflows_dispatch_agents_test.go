package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

type wfDispatchListing struct {
	Changed  bool
	Dispatch []WorkflowDispatchProfileStatus
	Agents   []string
}

func wfDispatchOf(t *testing.T, list wfDispatchListing, profile string) WorkflowDispatchProfileStatus {
	t.Helper()
	for _, d := range list.Dispatch {
		if d.Profile == profile {
			return d
		}
	}
	t.Fatalf("no dispatch entry for %s in %+v", profile, list.Dispatch)
	return WorkflowDispatchProfileStatus{}
}

// GET /api/workflows says which agent fills each bound model's profiles, or
// why none does, and PUT /api/workflows/dispatch/profiles saves the owner's
// pick as workflow.dispatch.profiles.<profile>.agent (hivecommons/hive#18690).
func TestWorkflowDispatchProfilePin(t *testing.T) {
	const writer = "techdocs-writer"
	s := workflowTestServer(t, true)
	cfg := s.deps.Config
	// At this level the scanner's sessions may open pull requests.
	level := 5
	cfg.ACMMLevel = &level
	if rec := wfReq(s, http.MethodPost, "/api/workflows", techdocsModel(t), true); rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	list := func() wfDispatchListing {
		t.Helper()
		rec := wfReq(s, http.MethodGet, "/api/workflows", nil, false)
		var out wfDispatchListing
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Dispatch == nil || out.Agents == nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return out
	}
	if got := list(); len(got.Dispatch) != 0 {
		t.Fatalf("an unbound hive lists dispatch profiles: %+v", got.Dispatch)
	}

	// Binding answers with the profiles too, so the row can warn at once.
	rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1", "name": "cncf-techdocs", "version": "1"}, true)
	var bound wfDispatchListing
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &bound) != nil {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body)
	}
	// The standard issue worker fills the PR-writing profile out of the box.
	if d := wfDispatchOf(t, bound, writer); d.Repo != "repo1" || d.Agent != "scanner" || d.Source != config.WorkflowDispatchSourceFallback || d.Problem != "" || len(d.States) == 0 {
		t.Fatalf("bound writer profile = %+v", d)
	}

	if d := wfDispatchOf(t, bound, writer); d.Mode != "ISSUES_AND_PRS" || len(d.Candidates) != 1 || !d.Candidates[0].Qualifies {
		t.Fatalf("bound writer profile mode and candidates = %+v", d)
	}

	// No agent can fill it: the only enabled agent is advisory, and the
	// entry carries that reason, not silence (hivecommons/hive#18749).
	scanner := cfg.Agents["scanner"]
	scanner.Enabled = false
	cfg.Agents["scanner"] = scanner
	cfg.Agents["guide"] = config.AgentConfig{Enabled: true, Mode: "ADVISORY"}
	got := list()
	if d := wfDispatchOf(t, got, writer); d.Agent != "" || d.Problem != "no agent on this hive may open pull requests in repo1; agents are advisory-only" {
		t.Fatalf("unfilled writer profile = %+v", d)
	}
	if len(got.Agents) != 1 || got.Agents[0] != "guide" {
		t.Fatalf("picker agents = %v", got.Agents)
	}
	// The picker is told which agents do not qualify, and why.
	if c := wfDispatchOf(t, got, writer).Candidates; len(c) != 1 || c[0].Agent != "guide" || c[0].Qualifies || c[0].Mode != "ADVISORY" || !strings.Contains(c[0].Reason, "may not open pull requests in repo1") {
		t.Fatalf("candidates = %+v", c)
	}
	// A pin to the advisory agent is saved and flagged: never a silent
	// pick, never a fallback.
	rec = wfReq(s, http.MethodPut, "/api/workflows/dispatch/profiles", map[string]string{"profile": writer, "agent": "guide"}, true)
	var flagged wfDispatchListing
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &flagged) != nil {
		t.Fatalf("advisory pin: %d %s", rec.Code, rec.Body)
	}
	if d := wfDispatchOf(t, flagged, writer); d.Agent != "" || d.Pinned != "guide" || d.Problem != `the pinned agent "guide" may not open pull requests in repo1 (mode ADVISORY)` {
		t.Fatalf("advisory pin = %+v", d)
	}
	if rec = wfReq(s, http.MethodPut, "/api/workflows/dispatch/profiles", map[string]string{"profile": writer, "agent": ""}, true); rec.Code != http.StatusOK {
		t.Fatalf("clearing the advisory pin: %d %s", rec.Code, rec.Body)
	}
	// The owner raises the agent's mode; from here it can fill the profile.
	cfg.Agents["guide"] = config.AgentConfig{Enabled: true, Mode: "ISSUES_AND_PRS"}

	pin := map[string]string{"profile": writer, "agent": "guide"}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/dispatch/profiles", pin, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner pin: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []any{"{", map[string]string{"agent": "guide"}, map[string]string{"profile": writer, "agent": "nobody"}, map[string]string{"profile": writer, "agent": "scanner"}} {
		if rec := wfReq(s, http.MethodPut, "/api/workflows/dispatch/profiles", bad, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad pin %v: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if cfg.Workflow.Dispatch.Profiles != nil {
		t.Fatalf("a refused pin wrote the config: %+v", cfg.Workflow.Dispatch.Profiles)
	}

	rec = wfReq(s, http.MethodPut, "/api/workflows/dispatch/profiles", pin, true)
	var pinned wfDispatchListing
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &pinned) != nil || !pinned.Changed {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
	if got := cfg.Workflow.Dispatch.Profile(writer).Agent; got != "guide" {
		t.Fatalf("workflow.dispatch.profiles.%s.agent = %q; want guide", writer, got)
	}
	if d := wfDispatchOf(t, pinned, writer); d.Agent != "guide" || d.Pinned != "guide" || d.Source != config.WorkflowDispatchSourcePinned || d.Problem != "" {
		t.Fatalf("pinned writer profile = %+v", d)
	}
	if d := wfDispatchOf(t, list(), writer); d.Agent != "guide" {
		t.Fatalf("the pin is not in the listing: %+v", d)
	}
	rec = wfReq(s, http.MethodPut, "/api/workflows/dispatch/profiles", pin, true)
	var again wfDispatchListing
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &again) != nil || again.Changed {
		t.Fatalf("same pin again: %d %s", rec.Code, rec.Body)
	}

	// Clearing the pin returns the profile to the default pick.
	rec = wfReq(s, http.MethodPut, "/api/workflows/dispatch/profiles", map[string]string{"profile": writer, "agent": ""}, true)
	if rec.Code != http.StatusOK || cfg.Workflow.Dispatch.Profile(writer).Agent != "" {
		t.Fatalf("unpin: %d %s", rec.Code, rec.Body)
	}
}

// The tracker's dispatch block carries the agent that fills each state's
// profile, or the reason none does, so the row can show it; and the manual
// send's error says what is missing and where to fix it.
func TestWorkflowDispatchVisibleReason(t *testing.T) {
	p := &workflowmodel.Profile{Dispatch: map[string]workflowmodel.DispatchTarget{
		"accepted": {Profile: "techdocs-drafter", State: "accepted", Produces: []string{"pull-request", "push"}},
		"drafting": {Profile: "techdocs-verifier", State: "drafting", Produces: []string{"comment"}},
	}}
	block := func() *StageTrackerWorkflow {
		return &StageTrackerWorkflow{Dispatch: map[string]StageTrackerWorkflowDispatch{
			"20": {Profile: "techdocs-drafter", State: "accepted"},
			"70": {Profile: "techdocs-verifier", State: "drafting"},
			"90": {Profile: "gone", State: "not-in-the-model"},
		}}
	}
	level := 5
	cfg := &config.Config{ACMMLevel: &level}
	cfg.Agents = map[string]config.AgentConfig{"scanner": {Enabled: true}, "reviewer": {Enabled: true}, "supervisor": {Enabled: true}}

	b := block()
	annotateWorkflowDispatch(cfg, "cncf/techdocs", p, b)
	if d := b.Dispatch["20"]; d.Agent != "scanner" || d.Problem != "" {
		t.Fatalf("drafter on a standard fleet = %+v", d)
	}
	if d := b.Dispatch["70"]; d.Agent != "reviewer" || d.Problem != "" {
		t.Fatalf("verifier on a standard fleet = %+v", d)
	}
	if d := b.Dispatch["90"]; d.Agent != "" || d.Problem != "" {
		t.Fatalf("a state the model does not dispatch was annotated: %+v", d)
	}

	adv := config.AgentConfig{Enabled: true, Mode: "ADVISORY"}
	cfg.Agents = map[string]config.AgentConfig{"supervisor": {Enabled: true}, "guide": adv, "brainstorm": adv, "scanner": adv, "reviewer": adv}
	b = block()
	annotateWorkflowDispatch(cfg, "cncf/techdocs", p, b)
	if d := b.Dispatch["20"]; d.Agent != "" || d.Problem != "no agent on this hive may open pull requests in cncf/techdocs; agents are advisory-only" {
		t.Fatalf("unfilled drafter = %+v", d)
	}
	if d := b.Dispatch["70"]; d.Agent != "" || d.Problem != "no agent on this hive may comment in cncf/techdocs; agents are advisory-only" {
		t.Fatalf("unfilled verifier = %+v", d)
	}
	raw, err := json.Marshal(b.Dispatch["20"])
	if err != nil || !strings.Contains(string(raw), `"problem":"no agent on this hive may open pull requests in cncf/techdocs; agents are advisory-only"`) {
		t.Fatalf("the row payload does not carry the reason: %s %v", raw, err)
	}
	annotateWorkflowDispatch(nil, "cncf/techdocs", p, b)
	annotateWorkflowDispatch(cfg, "cncf/techdocs", nil, b)
	annotateWorkflowDispatch(cfg, "cncf/techdocs", p, nil)

	res := cfg.ResolveWorkflowDispatch("techdocs-drafter", config.WorkflowDispatchNeed{Repo: "cncf/techdocs"}, true)
	sendErr := WorkflowDispatchNoAgentError(res)
	if !errors.Is(sendErr, ErrWorkflowDispatchNoAgent) || workflowDispatchStatus(sendErr) != http.StatusConflict {
		t.Fatalf("send error %v does not map to a conflict", sendErr)
	}
	for _, want := range []string{"no agent on this hive may open pull requests in cncf/techdocs; agents are advisory-only", "Settings → Repos", "workflow.dispatch.profiles.techdocs-drafter.agent"} {
		if !strings.Contains(sendErr.Error(), want) {
			t.Errorf("send error %q does not say %q", sendErr, want)
		}
	}
	if got := WorkflowDispatchNoAgentError(config.WorkflowDispatchResolution{Profile: "p"}).Error(); !strings.Contains(got, "no agent fills p") {
		t.Fatalf("send error with no reason = %q", got)
	}
	if got := workflowDispatchProfiles(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("nil config profiles = %v", got)
	}
}

// The shipped page: the Settings → Repos workflow row draws one picker per
// profile and warns when nothing fills it, the picker saves through PUT
// /api/workflows/dispatch/profiles, and the Activity Monitor row wears the
// reason.
func TestWorkflowDispatchGapUI(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function workflowDispatchProfileView(d, agents)`,
		`function renderRepoWorkflowProfileRows(repo, profiles, agents, locked)`,
		`renderRepoWorkflowProfileRows(repo, workflowDispatchProfilesFor(bound, _workflowState.dispatch), _workflowState.agents, locked)`,
		`data-change-action="setWorkflowProfileAgent"`,
		`async function setWorkflowProfileAgent(profile, value)`,
		`fetch('/api/workflows/dispatch/profiles', {`,
		`body: JSON.stringify({ profile: profile, agent: agent }),`,
		`function stageTrackerWorkflowGapHtml(item, workflows)`,
		`stageTrackerWorkflowGapHtml(item, ctx.workflows)`,
		`.ham-badge-wf-gap {`,
		`.wf-dispatch-gap {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing workflow-dispatch gap wiring %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const assert = require('assert');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
` + jsConstLine(t, html, "WORKFLOW_DISPATCH_SOURCE_WORD") + "\n" +
		jsFunc(t, html, "workflowDispatchProfilesFor") + jsFunc(t, html, "workflowDispatchProfileView") + jsFunc(t, html, "renderRepoWorkflowProfileRows") +
		jsFunc(t, html, "stageTrackerWorkflowOf") + jsFunc(t, html, "stageTrackerWorkflowDispatchOf") + jsFunc(t, html, "stageTrackerWorkflowDispatchAction") +
		jsFunc(t, html, "stageTrackerWorkflowGapOf") + jsFunc(t, html, "stageTrackerWorkflowGapHtml") + `
const agents = ['guide', 'reviewer', 'scanner'];
const dispatch = [
  { repo: 'cncf/techdocs', profile: 'techdocs-drafter', problem: 'no agent fills techdocs-drafter: this hive has no enabled writer agent' },
  { repo: 'cncf/techdocs', profile: 'techdocs-verifier', agent: 'reviewer', source: 'fallback' },
  { repo: 'cncf/other', profile: 'elsewhere', agent: 'scanner', source: 'fallback' },
];
const mine = workflowDispatchProfilesFor({ repo: 'CNCF/techdocs' }, dispatch);
assert.deepStrictEqual(mine.map(d => d.profile), ['techdocs-drafter', 'techdocs-verifier']);
assert.deepStrictEqual(workflowDispatchProfilesFor(null, dispatch), []);

// Nothing fills the drafter: a warning that names the profile, and a
// picker whose default says nothing is available.
const gap = workflowDispatchProfileView(mine[0], agents);
assert.ok(gap.warning.indexOf('no agent fills techdocs-drafter') >= 0, gap.warning);
assert.strictEqual(gap.note, '');
assert.deepStrictEqual(gap.options.map(o => o.value), ['', 'guide', 'reviewer', 'scanner']);
assert.strictEqual(gap.options[0].label, 'Automatic (none available)');
assert.strictEqual(gap.options[0].selected, true);

// The verifier resolved by the last resort: no warning, the default names it.
const ok = workflowDispatchProfileView(mine[1], agents);
assert.strictEqual(ok.warning, '');
assert.strictEqual(ok.options[0].label, 'Automatic (reviewer)');
assert.ok(ok.note.indexOf('handed to reviewer') === 0, ok.note);

// A pin selects its agent; one that no longer names an enabled agent stays visible.
const pinned = workflowDispatchProfileView({ profile: 'techdocs-drafter', agent: 'scanner', pinned: 'scanner', source: 'pinned' }, agents);
assert.deepStrictEqual(pinned.options.filter(o => o.selected).map(o => o.value), ['scanner']);
assert.strictEqual(pinned.options[0].label, 'Automatic');
const stale = workflowDispatchProfileView({ profile: 'techdocs-drafter', pinned: 'writer', problem: 'the pinned agent "writer" is not an enabled agent of this hive' }, agents);
assert.deepStrictEqual(stale.options.filter(o => o.selected).map(o => o.value), ['writer']);
assert.ok(stale.warning.indexOf('writer') >= 0);

const rows = renderRepoWorkflowProfileRows('techdocs', mine, agents, false);
assert.ok(rows.indexOf('data-change-action="setWorkflowProfileAgent" data-arg0="techdocs-drafter" data-arg-types="s,v"') >= 0, rows);
assert.ok(rows.indexOf('class="rt-inherit wf-dispatch-gap" role="alert"') >= 0, rows);
assert.strictEqual(rows.split('wf-dispatch-gap').length - 1, 1, 'only the unfilled profile warns');
assert.ok(rows.indexOf('<select disabled') < 0);
assert.ok(renderRepoWorkflowProfileRows('techdocs', mine, agents, true).indexOf('<select disabled') >= 0, 'a non-owner cannot pick');
assert.strictEqual(renderRepoWorkflowProfileRows('techdocs', [], agents, false), '');

// The Activity Monitor row: a short reason instead of silence.
const td = { name: 'cncf-techdocs-assessment', version: '1',
  columns: [{ id: 'accepted', label: 'Accepted', states: ['accepted'], numbers: [20] }, { id: 'drafting', label: 'Drafting', states: ['drafting'], numbers: [70] }],
  dispatch: { '20': { profile: 'techdocs-drafter', state: 'accepted', kinds: ['issue'], problem: 'no agent fills techdocs-drafter: this hive has no enabled writer agent' },
              '70': { profile: 'techdocs-verifier', state: 'drafting', kinds: ['pr'], reply: '/verify', agent: 'reviewer' } } };
const workflows = { default: { name: 'hive-default', version: '1', columns: [{ id: 'open', label: 'Open', states: ['open'], numbers: [] }] }, repos: { 'cncf/techdocs': td } };
const accepted = { repo: 'cncf/techdocs', kind: 'issue', number: 395, stage: 20 };
const g = stageTrackerWorkflowGapOf(accepted, workflows);
assert.strictEqual(g.word, 'no agent for techdocs-drafter');
assert.ok(g.tip.indexOf('Not handed off: no agent fills techdocs-drafter') === 0, g.tip);
assert.ok(g.tip.indexOf('Settings → Repos') >= 0, g.tip);
const badge = stageTrackerWorkflowGapHtml(accepted, workflows);
assert.ok(badge.indexOf('class="ham-badge ham-badge-wf-gap"') >= 0 && badge.indexOf('no agent for techdocs-drafter') >= 0, badge);
assert.strictEqual(stageTrackerWorkflowDispatchAction(accepted, workflows, { owner: true }).hint, 'no agent fills it');
const draft = { repo: 'cncf/techdocs', kind: 'pr', number: 400, stage: 70 };
assert.strictEqual(stageTrackerWorkflowGapOf(draft, workflows), null, 'a filled profile says nothing');
assert.strictEqual(stageTrackerWorkflowGapHtml(draft, workflows), '');
assert.strictEqual(stageTrackerWorkflowDispatchOf(draft, workflows).agent, 'reviewer');
assert.strictEqual(stageTrackerWorkflowGapHtml({ repo: 'cncf/other', kind: 'issue', number: 1, stage: 20 }, workflows), '', 'unbound repo');
assert.strictEqual(stageTrackerWorkflowGapHtml(accepted, null), '');
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node workflow dispatch gap fixture failed: %v\n%s", err, out)
	}
}
