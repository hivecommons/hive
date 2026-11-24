package scheduler

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub/marketplace/curated"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// fakeWorkflowLookup owns issue 395 (Accepted → drafter) and draft PR 400
// (Drafting → verifier on /verify) of cncf/techdocs; everything else,
// including every other repository, is unowned.
type fakeWorkflowLookup struct{}

func drafterDispatch() github.WorkflowDispatch {
	return github.WorkflowDispatch{
		Repo: "cncf/techdocs", Number: 395, Kind: workflowmodel.KindIssue, Title: "Assess X",
		URL: "https://github.com/cncf/techdocs/issues/395", Labels: []string{"Docs analysis", "phase/assessment", "triage/accepted"},
		Model: "cncf-techdocs-assessment@curated-2026.11.2", State: "accepted",
		Target: workflowmodel.DispatchTarget{
			Profile: "techdocs-drafter", State: "accepted", StateLabel: "Accepted",
			Produces:     []string{"pull-request", "push"},
			Instructions: []string{"reviewer to delegate the phase drafter"},
			Deliverable:  &workflowmodel.Deliverable{State: "drafting", Draft: true, Labels: []string{"Docs analysis"}},
		},
		Profile: &workflowmodel.Profile{
			GuardedLabels: []string{"triage/accepted", "verified", "confirmed"},
			Labels: workflowmodel.LabelSet{Entries: []workflowmodel.LabelEntry{
				{Name: "Docs analysis"}, {Name: "phase/assessment"}, {Name: "triage/accepted", State: "accepted"}, {Name: "verified", State: "in-review"},
			}},
		},
	}
}

func verifierDispatch() github.WorkflowDispatch {
	return github.WorkflowDispatch{
		Repo: "cncf/techdocs", Number: 400, Kind: workflowmodel.KindPR, Title: "Phase A draft",
		Model: "cncf-techdocs-assessment@curated-2026.11.2", State: "drafting",
		Target:  workflowmodel.DispatchTarget{Profile: "techdocs-verifier", State: "drafting", Reply: "/verify", Produces: []string{"comment"}},
		Profile: &workflowmodel.Profile{GuardedLabels: []string{"verified"}},
	}
}

func (fakeWorkflowLookup) WorkflowDispatchForIssue(issue github.Issue) (github.WorkflowDispatch, bool) {
	if issue.Repo == "cncf/techdocs" && issue.Number == 395 {
		return drafterDispatch(), true
	}
	return github.WorkflowDispatch{}, false
}

func (fakeWorkflowLookup) WorkflowDispatchForPR(pr github.PullRequest) (github.WorkflowDispatch, bool) {
	if pr.Repo == "cncf/techdocs" && pr.Number == 400 {
		return verifierDispatch(), true
	}
	return github.WorkflowDispatch{}, false
}

// Generic lanes never see a model-owned item; unbound repositories and
// unowned items pass through, and the input is not mutated.
func TestWithholdWorkflowOwned(t *testing.T) {
	in := &github.ActionableResult{}
	in.Issues.Items = []github.Issue{{Repo: "cncf/techdocs", Number: 395}, {Repo: "cncf/techdocs", Number: 396}, {Repo: "acme/widgets", Number: 395}}
	in.Issues.Count = 3
	in.PRs.Items = []github.PullRequest{{Repo: "cncf/techdocs", Number: 400}, {Repo: "acme/widgets", Number: 9}}
	in.PRs.Count = 2

	out, withheld := WithholdWorkflowOwned(in, fakeWorkflowLookup{})
	if withheld != 2 {
		t.Fatalf("withheld %d; want 2", withheld)
	}
	if len(out.Issues.Items) != 2 || out.Issues.Count != 2 || out.Issues.Items[0].Number != 396 || out.Issues.Items[1].Repo != "acme/widgets" {
		t.Fatalf("issues %+v", out.Issues)
	}
	if len(out.PRs.Items) != 1 || out.PRs.Count != 1 || out.PRs.Items[0].Repo != "acme/widgets" {
		t.Fatalf("prs %+v", out.PRs)
	}
	if len(in.Issues.Items) != 3 || len(in.PRs.Items) != 2 || in.Issues.Count != 3 {
		t.Fatal("input was mutated")
	}

	// Nothing owned: the same result comes back.
	plain := &github.ActionableResult{}
	plain.Issues.Items = []github.Issue{{Repo: "acme/widgets", Number: 1}}
	if got, n := WithholdWorkflowOwned(plain, fakeWorkflowLookup{}); got != plain || n != 0 {
		t.Fatalf("unowned set changed: %v %d", got, n)
	}
	if got, n := WithholdWorkflowOwned(plain, nil); got != plain || n != 0 {
		t.Fatal("nil lookup changed the set")
	}
}

func planDeps(done map[string]bool) WorkflowDispatchPlanDeps {
	return WorkflowDispatchPlanDeps{
		Agent: func(d github.WorkflowDispatch) (string, bool) { return "writer", d.Target.Profile != "" },
		Done:  func(key string) bool { return done[key] },
	}
}

// The drafter is handed an Accepted issue once; a second plan with the key
// recorded hands nothing.
func TestPlanWorkflowDispatchOnce(t *testing.T) {
	done := map[string]bool{}
	items := []github.WorkflowDispatch{drafterDispatch(), drafterDispatch()}
	cands, _ := PlanWorkflowDispatch(items, planDeps(done))
	if len(cands) != 1 || cands[0].Agent != "writer" || cands[0].Key != "cncf/techdocs#395|techdocs-drafter|accepted" {
		t.Fatalf("first plan %+v", cands)
	}
	done[cands[0].Key] = true
	cands, skips := PlanWorkflowDispatch(items, planDeps(done))
	if len(cands) != 0 || len(skips) != 1 || skips[0].Reason != WorkflowSkipDone {
		t.Fatalf("second plan %+v skips %+v", cands, skips)
	}
}

func TestPlanWorkflowDispatchGates(t *testing.T) {
	deps := planDeps(map[string]bool{})
	deps.Agent = func(github.WorkflowDispatch) (string, bool) { return "", false }
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{drafterDispatch()}, deps); len(cands) != 0 || skips[0].Reason != WorkflowSkipNoAgent || skips[0].Profile != "techdocs-drafter" {
		t.Fatalf("no agent: %+v %+v", cands, skips)
	}
	deps = planDeps(map[string]bool{})
	deps.Eligible = func(string) bool { return false }
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{drafterDispatch()}, deps); len(cands) != 0 || skips[0].Reason != WorkflowSkipIneligible {
		t.Fatalf("paused agent: %+v %+v", cands, skips)
	}
}

// The verifier waits for /verify; each new /verify comment is a new pass.
func TestPlanWorkflowDispatchReply(t *testing.T) {
	done := map[string]bool{}
	deps := planDeps(done)
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{verifierDispatch()}, deps); len(cands) != 0 || skips[0].Reason != WorkflowSkipNoReply {
		t.Fatalf("no reply lookup: %+v %+v", cands, skips)
	}
	var reply github.DispatchReply
	found := false
	deps.Reply = func(github.WorkflowDispatch) (github.DispatchReply, bool, error) { return reply, found, nil }
	if cands, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{verifierDispatch()}, deps); len(cands) != 0 || skips[0].Reason != WorkflowSkipNoReply {
		t.Fatalf("no /verify yet: %+v %+v", cands, skips)
	}
	reply, found = github.DispatchReply{CommentID: 11, Author: "writer", URL: "u"}, true
	cands, _ := PlanWorkflowDispatch([]github.WorkflowDispatch{verifierDispatch()}, deps)
	if len(cands) != 1 || cands[0].Key != "cncf/techdocs#400|techdocs-verifier|drafting|11" || cands[0].Trigger == nil {
		t.Fatalf("after /verify: %+v", cands)
	}
	done[cands[0].Key] = true
	if cands, _ := PlanWorkflowDispatch([]github.WorkflowDispatch{verifierDispatch()}, deps); len(cands) != 0 {
		t.Fatalf("same /verify dispatched twice: %+v", cands)
	}
	reply.CommentID = 12
	if cands, _ := PlanWorkflowDispatch([]github.WorkflowDispatch{verifierDispatch()}, deps); len(cands) != 1 {
		t.Fatalf("a new /verify must start a new pass: %+v", cands)
	}
	deps.Reply = func(github.WorkflowDispatch) (github.DispatchReply, bool, error) {
		return github.DispatchReply{}, false, errors.New("boom")
	}
	if _, skips := PlanWorkflowDispatch([]github.WorkflowDispatch{verifierDispatch()}, deps); !strings.HasPrefix(skips[0].Reason, WorkflowSkipReplyError) {
		t.Fatalf("reply error: %+v", skips)
	}
}

// A restart reopens the log and still knows what was dispatched.
func TestWorkflowDispatchLogSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow-dispatch.json")
	l, err := OpenWorkflowDispatchLog(path)
	if err != nil {
		t.Fatal(err)
	}
	key := WorkflowDispatchKey(drafterDispatch(), nil)
	if l.Done(key) {
		t.Fatal("fresh log already done")
	}
	if err := l.Record(WorkflowDispatchEntry{Key: key, Ref: "cncf/techdocs#395", Profile: "techdocs-drafter", State: "accepted", Agent: "writer", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWorkflowDispatchLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Done(key) || len(reopened.Entries()) != 1 {
		t.Fatalf("restart lost the record: %+v", reopened.Entries())
	}
	cands, _ := PlanWorkflowDispatch([]github.WorkflowDispatch{drafterDispatch()}, WorkflowDispatchPlanDeps{
		Agent: func(github.WorkflowDispatch) (string, bool) { return "writer", true },
		Done:  reopened.Done,
	})
	if len(cands) != 0 {
		t.Fatalf("dispatched again after restart: %+v", cands)
	}
	var nilLog *WorkflowDispatchLog
	if nilLog.Done(key) || nilLog.Record(WorkflowDispatchEntry{Key: key}) != nil {
		t.Fatal("nil log misbehaved")
	}
}

func TestBuildWorkflowDispatchPrompt(t *testing.T) {
	c := WorkflowDispatchCandidate{Dispatch: drafterDispatch(), Agent: "writer"}
	msg := BuildWorkflowDispatchPrompt(c, "")
	for _, want := range []string{
		"cncf/techdocs#395",
		"techdocs-drafter",
		"**Accepted**",
		"hive-open-pr --issues 395",
		"Refs #395",
		"DRAFT pull request labeled `Docs analysis`, `phase/assessment`",
		"Do not mark the pull request ready for review.",
		"reviewer to delegate the phase drafter",
		"claimed the issue for you",
		"`verified`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("prompt missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "${") {
		t.Errorf("unexpanded placeholder:\n%s", msg)
	}
	if strings.Contains(msg, "labeled `Docs analysis`, `phase/assessment`, `triage/accepted`") {
		t.Errorf("a gate label was carried over:\n%s", msg)
	}

	v := WorkflowDispatchCandidate{Dispatch: verifierDispatch(), Agent: "writer", Trigger: &github.DispatchReply{CommentID: 1, Author: "nate", URL: "https://x/c1"}}
	vmsg := BuildWorkflowDispatchPrompt(v, "")
	for _, want := range []string{"`/verify` from nate (https://x/c1)", "post the result as one comment", "Do not push commits"} {
		if !strings.Contains(vmsg, want) {
			t.Errorf("verifier prompt missing %q:\n%s", want, vmsg)
		}
	}

	custom := BuildWorkflowDispatchPrompt(c, "Draft ${REF} as ${PROFILE} for ${AGENT}")
	if custom != "Draft cncf/techdocs#395 as techdocs-drafter for writer" {
		t.Fatalf("template %q", custom)
	}
}

// The curated-2026.11.4 whose-turn labels are the hive's (hivecommons/hive#19609):
// both TechDocs profiles are told to leave them alone, and the gate labels
// keep their own "a person's gate" line instead.
func TestWorkflowDispatchPromptStatusLabels(t *testing.T) {
	entries, errs := curated.Load()
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	var p *workflowmodel.Profile
	for _, e := range entries {
		if e.Model.Metadata.Name == "cncf-techdocs-assessment" && e.Model.Spec.Meta.Version == "curated-2026.11.4" {
			var err error
			if p, _, err = workflowmodel.Compile(e.Model); err != nil {
				t.Fatal(err)
			}
		}
	}
	if p == nil {
		t.Fatal("cncf-techdocs-assessment@curated-2026.11.4 not embedded")
	}
	const line = "Do not add or remove `ready-for-techdocs-review`, `ready-for-stakeholder-review`, `ready-for-approver`: the hive sets those status labels from the item's state."
	for _, state := range []string{"accepted", "drafting"} {
		target, ok := p.DispatchFor(state)
		if !ok {
			t.Fatalf("no dispatch in %s", state)
		}
		d := drafterDispatch()
		d.Model, d.State, d.Target, d.Profile = "cncf-techdocs-assessment@curated-2026.11.4", state, target, p
		msg := BuildWorkflowDispatchPrompt(WorkflowDispatchCandidate{Dispatch: d, Agent: "writer"}, "")
		if !strings.Contains(msg, line) {
			t.Errorf("%s (%s) prompt missing the status-label line:\n%s", target.Profile, state, msg)
		}
		if !strings.Contains(msg, "`verified`") || strings.Contains(msg, "`verified`: the hive sets") {
			t.Errorf("%s: the verified gate must stay a person's gate, not a status label:\n%s", target.Profile, msg)
		}
	}
	// A model without status labels says nothing about them.
	if msg := BuildWorkflowDispatchPrompt(WorkflowDispatchCandidate{Dispatch: drafterDispatch(), Agent: "writer"}, ""); strings.Contains(msg, "status labels") {
		t.Errorf("status-label line without status labels:\n%s", msg)
	}
}
