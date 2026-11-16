package github

import (
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Workflow-model stage ownership (hivecommons/hive#18434).
//
// A repository bound to a workflow model derives its stage through the
// model (stageengine.DeriveForRepo). A model state reuses the built-in stage
// label for its lifecycle number, so the TechDocs "Accepted" state (number
// 20: waiting on the reviewer to delegate the drafter) wears
// stage/020-claimed although no agent holds a claim. The in-progress sweep
// read that label as a claim, found no hive:claim marker, and stripped it;
// the stage sync two seconds later derived the model state again and put it
// back. cncf/techdocs#395 flapped every enumeration cycle for a day and a
// half.
//
// Two writers now agree: the in-progress sweep leaves a stage label the
// bound model derives for the issue alone (and does not withhold the issue,
// since a model state is not a claim), and the out-of-cycle FactSink derives
// through the same per-repository model lookup as the sweeps.

// stageWorkflowLookup is the per-repository model lookup, held behind an
// atomic pointer because the sweeps read it while boot installs it.
type stageWorkflowLookup struct {
	fn func(owner, repo string) *workflowmodel.Profile
}

// SetStageWorkflowFor installs the per-repository workflow model lookup the
// stage engine uses (stageengine.Options.WorkflowFor). StageReconciler
// installs it from the options it is first built with, so cmd/hive's boot
// wiring needs nothing extra; tests call it directly. Nil clears it.
func (c *Client) SetStageWorkflowFor(fn func(owner, repo string) *workflowmodel.Profile) {
	if c == nil {
		return
	}
	if fn == nil {
		c.stageWorkflow.Store(nil)
		return
	}
	c.stageWorkflow.Store(&stageWorkflowLookup{fn: fn})
}

// stageWorkflowFor resolves the workflow model bound to owner/repo, or nil
// when none is installed or the repository is unbound.
func (c *Client) stageWorkflowFor(owner, repo string) *workflowmodel.Profile {
	if c == nil {
		return nil
	}
	l := c.stageWorkflow.Load()
	if l == nil || l.fn == nil {
		return nil
	}
	return l.fn(owner, repo)
}

// modelOwnsStageLabel reports whether the workflow model bound to the
// issue's repository derives a state for the issue whose stage label is
// label. Such a label is the model's state, not a claim: the in-progress
// sweep must neither strip it nor treat the issue as claimed. The built-in
// hive-default model never owns a label (it is the built-in derivation).
func (c *Client) modelOwnsStageLabel(issue Issue, label string) bool {
	owner, name := c.splitRepo(issue.Repo)
	p := c.stageWorkflowFor(owner, name)
	if p == nil || p.Name == hiveDefaultWorkflowName {
		return false
	}
	f := issueFactsFrom(issueStageInputFrom(issue), func(string, int) stagelabels.Stage { return stagelabels.None })
	f.Ref.Owner, f.Ref.Repo = owner, name
	d := stageengine.DeriveFromModel(p, stagelabels.IssueItem).Derive(f)
	if d.Abstain || d.ModelState == "" || d.Stage == stagelabels.None {
		return false
	}
	return d.Stage.Label() == label
}

// hiveDefaultWorkflowName is the built-in model's name
// (workflowmodel.Default); binding it changes nothing about derivation.
const hiveDefaultWorkflowName = "hive-default"
