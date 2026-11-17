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
