package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func workflowCandidate(kind workflowmodel.ItemKind, number int) scheduler.WorkflowDispatchCandidate {
	return scheduler.WorkflowDispatchCandidate{
		Dispatch: github.WorkflowDispatch{Repo: "cncf/techdocs", Number: number, Kind: kind, State: "accepted",
			Target: workflowmodel.DispatchTarget{Profile: "techdocs-drafter"}},
		Agent: "writer",
		Key:   "k",
	}
}

// The claim comes first: a free issue is claimed for the drafter's agent,
// an issue a person holds is not handed off, and a pull request is never
// claimed as an issue.
func TestClaimForWorkflowDispatch(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "cncf"
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !claimForWorkflowDispatch(cfg, l, workflowCandidate(workflowmodel.KindIssue, 395), nil) {
		t.Fatal("free issue not handed off")
	}
	c, ok := l.Lookup("cncf/techdocs", 395)
	if !ok || c.Holder != "writer" || c.Kind != claims.KindAgent {
		t.Fatalf("claim %+v %v", c, ok)
	}
	// Same agent again (a retry after a deferred kick) renews.
	if !claimForWorkflowDispatch(cfg, l, workflowCandidate(workflowmodel.KindIssue, 395), nil) {
		t.Fatal("renewal refused")
	}

	if _, err := l.Claim(claims.Request{Repo: "cncf/techdocs", Issue: 396, Holder: "nate", Kind: claims.KindHuman}); err != nil {
		t.Fatal(err)
	}
	if claimForWorkflowDispatch(cfg, l, workflowCandidate(workflowmodel.KindIssue, 396), nil) {
		t.Fatal("an issue a person holds was handed off")
	}

	if !claimForWorkflowDispatch(cfg, l, workflowCandidate(workflowmodel.KindPR, 400), nil) {
		t.Fatal("pull request hand-off refused")
	}
	if _, ok := l.Lookup("cncf/techdocs", 400); ok {
		t.Fatal("a pull request was claimed as an issue")
	}
}

func TestWorkflowDispatchPendingLapses(t *testing.T) {
	rt := &workflowDispatchRuntime{pending: map[string]time.Time{}, replies: map[string]workflowReplyCacheEntry{}}
	now := time.Now()
	rt.markPending("k", now)
	if !rt.inFlight("k", now.Add(time.Minute)) {
		t.Fatal("fresh hand-off not in flight")
	}
	if rt.inFlight("k", now.Add(workflowDispatchPendingTTL+time.Minute)) {
		t.Fatal("in-flight mark never lapses")
	}
	rt.markPending("k", now)
	rt.clearPending("k")
	if rt.inFlight("k", now) {
		t.Fatal("cleared hand-off still in flight")
	}
}

func TestWorkflowDispatchActive(t *testing.T) {
	off := false
	cfg := &config.Config{}
	if workflowDispatchActive(cfg) {
		t.Fatal("active with workflow.enabled off")
	}
	cfg.Workflow.Enabled = true
	if !workflowDispatchActive(cfg) {
		t.Fatal("dispatch must default on")
	}
	cfg.Workflow.Dispatch.Enabled = &off
	if workflowDispatchActive(cfg) {
		t.Fatal("workflow.dispatch.enabled: false ignored")
	}
	if workflowDispatchActive(nil) {
		t.Fatal("nil config active")
	}
}

// On a standard fleet (no writer, docs or feature agent) the drafter's
// item goes to the scanner and the verifier's to the reviewer, so the
// TechDocs workflow starts out of the box; an agent scoped away from the
// item's repository is not picked (hivecommons/hive#18690).
func TestResolveWorkflowDispatchAgentStandardFleet(t *testing.T) {
	level := 5
	cfg := &config.Config{ACMMLevel: &level}
	cfg.Project.Org = "cncf"
	cfg.Agents = map[string]config.AgentConfig{}
	for _, name := range []string{"brainstorm", "ci-maintainer", "guide", "quality", "reviewer", "scanner", "supervisor"} {
		cfg.Agents[name] = config.AgentConfig{Enabled: true}
	}
	drafter := github.WorkflowDispatch{Repo: "techdocs", Number: 395, Kind: workflowmodel.KindIssue, State: "accepted",
		Target: workflowmodel.DispatchTarget{Profile: "techdocs-drafter", Produces: []string{"pull-request", "push"}}}
	verifier := github.WorkflowDispatch{Repo: "techdocs", Number: 400, Kind: workflowmodel.KindPR, State: "drafting",
		Target: workflowmodel.DispatchTarget{Profile: "techdocs-verifier", Reply: "/verify", Produces: []string{"comment"}}}
	if got := resolveWorkflowDispatchAgent(cfg, drafter, false); got.Agent != "scanner" {
		t.Fatalf("drafter = %+v; want scanner", got)
	}
	if got := resolveWorkflowDispatchAgent(cfg, verifier, false); got.Agent != "reviewer" {
		t.Fatalf("verifier = %+v; want reviewer", got)
	}
	cands, skips := scheduler.PlanWorkflowDispatch([]github.WorkflowDispatch{drafter, verifier}, scheduler.WorkflowDispatchPlanDeps{
		Agent: func(d github.WorkflowDispatch) (string, bool) {
			r := resolveWorkflowDispatchAgent(cfg, d, false)
			return r.Agent, r.OK()
		},
	})
	if len(cands) != 1 || cands[0].Agent != "scanner" || len(skips) != 1 || skips[0].Reason != scheduler.WorkflowSkipNoReply {
		t.Fatalf("plan = %+v skips %+v", cands, skips)
	}

	// With the scanner scoped away and no other agent allowed to open pull
	// requests, nothing fills the drafter.
	for _, name := range []string{"brainstorm", "ci-maintainer", "guide", "quality", "reviewer"} {
		cfg.Agents[name] = config.AgentConfig{Enabled: true, Mode: "ADVISORY"}
	}
	scoped := cfg.Agents["scanner"]
	scoped.Repos = []string{"cncf/website"}
	cfg.Agents["scanner"] = scoped
	got := resolveWorkflowDispatchAgent(cfg, drafter, true)
	if got.OK() || got.Problem == "" {
		t.Fatalf("a scanner scoped to another repository was picked: %+v", got)
	}
	_, skips = scheduler.PlanWorkflowDispatch([]github.WorkflowDispatch{drafter}, scheduler.WorkflowDispatchPlanDeps{
		Agent: func(d github.WorkflowDispatch) (string, bool) {
			r := resolveWorkflowDispatchAgent(cfg, d, false)
			return r.Agent, r.OK()
		},
	})
	if len(skips) != 1 || skips[0].Reason != scheduler.WorkflowSkipNoAgent || skips[0].Profile != "techdocs-drafter" {
		t.Fatalf("skips = %+v", skips)
	}
}
