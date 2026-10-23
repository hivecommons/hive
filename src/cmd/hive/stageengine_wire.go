package main

import (
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stageengine"
)

// Stage engine (ADR-0025) — composition root. The engine's DeriveIssue /
// DerivePR delegate to the kind-specific derivers: the enumeration-backed
// issue deriver in pkg/github and the lane-backed PR deriver in
// pkg/github/automerge. Each package registers its own at init; this
// re-registration pins the pairing at the hub where both are linked, so
// the github.Client's default FactSink can never see a PR fact abstain for
// want of a deriver.
func init() {
	stageengine.SetDerivers(github.IssueDeriver(), automerge.PRDeriver())
}
