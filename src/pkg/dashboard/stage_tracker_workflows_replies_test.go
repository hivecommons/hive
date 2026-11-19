package dashboard

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// The tracker's workflow block carries the model's human-gate replies per
// stage number (hivecommons/hive#17620, RFC phase 4); hive-default's block
// stays as it was.
func TestStageTrackerWorkflowBlockCarriesReplies(t *testing.T) {
	s := workflowTestServer(t, true)
	if rec := wfReq(s, http.MethodPost, "/api/workflows", techdocsModel(t), true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	if _, err := s.deps.Config.SetWorkflowBinding("repo1", "cncf-techdocs", "1"); err != nil {
		t.Fatal(err)
	}
	blk := stageTrackerWorkflows(s.deps.Config, s.workflowProfileLookup(), func(r string) string { return "myorg/" + r })
	if blk == nil || blk.Default.Replies != nil {
		t.Fatalf("hive-default must carry no comment verbs: %+v", blk)
	}
	td := blk.Repos["myorg/repo1"]
	if got := strings.Join(td.Replies["45"], ","); got != "approve analysis,revise" {
		t.Fatalf("replies[45] = %q (%+v)", got, td.Replies)
	}
	if got := strings.Join(td.Replies["20"], ","); got != "hand back" {
		t.Fatalf("replies[20] = %q", got)
	}
	if _, ok := td.Replies["10"]; ok {
		t.Fatalf("a state without replies must be absent: %+v", td.Replies)
	}
}

func TestWorkflowProfileForRepo(t *testing.T) {
	s := workflowTestServer(t, true)
	if rec := wfReq(s, http.MethodPost, "/api/workflows", techdocsModel(t), true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	if _, err := s.deps.Config.SetWorkflowBinding("repo1", "cncf-techdocs", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deps.Config.SetWorkflowBinding("repo2", "ghost", "1"); err != nil {
		t.Fatal(err)
	}
	org := s.deps.Config.Project.Org
	s.deps.Config.Workflow.Enabled = false
	if s.workflowProfileForRepo(org+"/repo1") != nil {
		t.Fatal("workflow.enabled=false must resolve nothing")
	}
	s.deps.Config.Workflow.Enabled = true
	if p := s.workflowProfileForRepo(org + "/repo1"); p == nil || p.Name != "cncf-techdocs" {
		t.Fatalf("bound repo: %+v", p)
	}
	for _, repo := range []string{org + "/repo2", org + "/unbound", ""} {
		if p := s.workflowProfileForRepo(repo); p != nil {
			t.Errorf("%q resolved %s", repo, p.Name)
		}
	}
	s.deps.WorkflowStore = nil
	if s.workflowProfileForRepo(org+"/repo1") != nil {
		t.Fatal("no store must resolve nothing")
	}
}

// A model reply from the dashboard records `/hive <verb>` on the parked
// issue and applies the label the model maps; unbound repositories and
// verbs the model does not offer are refused before anything is written.
func TestRepoItemDecisionModelReply(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"stage/045-needs-decision"}}
	srv := newRepoItemDecisionServer(t, mock)
	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"model-reply","reply":"revise"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not bound") {
		t.Fatalf("unbound: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"model-reply"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("blank verb: code=%d", w.Code)
	}

	store := wfstore.Open(t.TempDir())
	if _, err := store.Save(techdocsModel(t), "andy", time.Now()); err != nil {
		t.Fatal(err)
	}
	srv.deps.WorkflowStore = store
	srv.deps.Config.Workflow.Enabled = true
	if _, err := srv.deps.Config.SetWorkflowBinding("testrepo", "cncf-techdocs", "1"); err != nil {
		t.Fatal(err)
	}
	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"model-reply","reply":"ship it"}`); w.Code != http.StatusConflict {
		t.Fatalf("unknown verb: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"model-reply","reply":"revise","type":"pr"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("pr: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.added)+len(mock.comments) != 0 {
		t.Fatalf("refusals must write nothing: added=%v comments=%v", mock.added, mock.comments)
	}

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"model-reply","reply":"Revise","comment":"section 2 is thin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("model reply: code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"status":"unparked"`) || !strings.Contains(body, "Workflow reply recorded as `/hive revise: section 2 is thin`") {
		t.Fatalf("response = %s", body)
	}
	added := strings.Join(mock.added, ",")
	if !strings.Contains(added, "approved-direction") || !strings.Contains(added, "techdocs/analysis") {
		t.Fatalf("expected the ack and the model's label applied, got %v", mock.added)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Workflow reply `revise` moves the item from `analysis-review` to `analysing`; `techdocs/analysis` applied.") || !strings.Contains(mock.comments[0], "hive dashboard") {
		t.Fatalf("comments = %v", mock.comments)
	}
	actions := strings.Join(decisionAuditActions(srv), ",")
	if !strings.Contains(actions, auditRepoItemDecisionModelReply) {
		t.Fatalf("audit = %s", actions)
	}
}

// The gate pill offers the model's verbs after the built-ins, each as a
// `wf:<verb>` action that posts a model-reply decision on the issue.
func TestStageTrackerHumanGateModelVerbsWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function stageTrackerWorkflowReplies(item, workflows)`,
		`function stageTrackerHumanGateVerbs(reasons, item, modelVerbs)`,
		`const verbs = stageTrackerHumanGateVerbs(reasons, shown, modelVerbs);`,
		`body: { decision: 'model-reply', reply: modelVerb, comment: payload.comment || '', type } };`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing model-verb wiring %q", want)
		}
	}
}

func TestStageTrackerHumanGateModelVerbsNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
const STAGE_TRACKER_GATE_VERBS = { decision: ['gate:decide', 'decide:approve', 'decide:delegate'], hold: ['release', 'decide:delegate'] };
const STAGE_TRACKER_GATE_PR_ONLY = ['decide:approve', 'gate:request-changes'];
const STAGE_TRACKER_GATE_LABELS = { 'decide:approve': 'Approve', 'decide:delegate': 'Let hive decide', 'gate:decide': 'Decide…', release: 'Release hold' };
const STAGE_TRACKER_GATE_APPROVE_AS_IS = 'Approve as-is';
const STAGE_TRACKER_GATE_POSTS = {};
const STAGE_TRACKER_GATE_MODEL_PREFIX = 'wf:';
function stageTrackerGateIsPR(item) { return !!item && (item.via_pr ? 'pr' : item.kind) === 'pr'; }
` + jsFunc(t, html, "stageTrackerWorkflowOf") + jsFunc(t, html, "stageTrackerWorkflowReplies") + jsFunc(t, html, "stageTrackerWorkflowReplyLabel") +
		jsFunc(t, html, "stageTrackerHumanGateVerbs") + jsFunc(t, html, "stageTrackerModelReplyVerb") + `
const td = { name: 'cncf-techdocs', version: '1', columns: [{ id: 'a', label: 'A', states: ['analysis-review'], numbers: [45] }], replies: { '45': ['approve analysis', 'revise', 'decide', ' '], '20': ['hand back'] } };
const def = { name: 'hive-default', version: '1', columns: [{ id: 'open', label: 'Open', states: ['open'], numbers: [] }], replies: { '45': ['decide:approve'] } };
const workflows = { default: def, repos: { 'cncf/techdocs': td, 'cncf/plain': def } };
const issue = { repo: 'cncf/techdocs', kind: 'issue', stage: 45 };
assert.deepStrictEqual(stageTrackerWorkflowReplies(issue, workflows), ['approve analysis', 'revise', 'decide']);
assert.deepStrictEqual(stageTrackerWorkflowReplies({ repo: 'cncf/techdocs', kind: 'issue', stage: 10 }, workflows), [], 'no replies at this state');
assert.deepStrictEqual(stageTrackerWorkflowReplies({ repo: 'cncf/plain', kind: 'issue', stage: 45 }, workflows), [], 'hive-default offers none');
assert.deepStrictEqual(stageTrackerWorkflowReplies({ repo: 'cncf/other', kind: 'issue', stage: 45 }, workflows), [], 'unbound');
assert.deepStrictEqual(stageTrackerWorkflowReplies({ repo: 'cncf/techdocs', kind: 'pr', stage: 45 }, workflows), [], 'PR rows take no model verb');
assert.deepStrictEqual(stageTrackerWorkflowReplies(issue, null), []);

const reasons = [{ key: 'decision', tip: 'q' }];
const plain = stageTrackerHumanGateVerbs(reasons, issue);
assert.deepStrictEqual(plain.map(v => v.id), ['gate:decide', 'decide:delegate'], 'no model verbs → unchanged');
const verbs = stageTrackerHumanGateVerbs(reasons, issue, stageTrackerWorkflowReplies(issue, workflows));
assert.deepStrictEqual(verbs.map(v => v.id), ['gate:decide', 'decide:delegate', 'wf:approve analysis', 'wf:revise'], 'model verbs follow the built-ins; "decide" collides with Decide… and is dropped');
assert.strictEqual(verbs[2].label, 'Approve analysis');
assert.ok(verbs[2].posts.includes('/hive approve analysis'), verbs[2].posts);
assert.strictEqual(verbs[3].model, true);
assert.strictEqual(stageTrackerModelReplyVerb('wf:approve analysis'), 'approve analysis');
assert.strictEqual(stageTrackerModelReplyVerb('decide:approve'), '');
assert.strictEqual(stageTrackerWorkflowReplyLabel(''), '');
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node model verbs fixture failed: %v\n%s", err, out)
	}
}
