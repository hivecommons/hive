package main

import (
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stageengine"
)

// Stage engine (ADR-0025) — composition root. The engine's DeriveIssue /
// DerivePR delegate to the kind-specific derivers registered here: the
// enumeration-backed issue deriver in pkg/github and the lane-backed PR
// deriver in pkg/github/automerge. Neither package can import the other
// (automerge imports github), so the hub is where both meet. The
// github.Client's default FactSink reads the PR side through this
// registration; without it a PR fact would abstain.
func init() {
	stageengine.SetDerivers(github.IssueDeriver(), automerge.PRDeriver())
}
