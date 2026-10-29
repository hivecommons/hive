package stageengine

import "github.com/hivecommons/hive/pkg/stagelabels"

// diagramEdges is the expected-transition table, generated from the
// `stateDiagram-v2` block in src/docs/labels-and-control-signals.md. One
// entry per `sNNN --> sMMM` line, in diagram order; `[*]` edges are
// implied by Expected (None is always expected). transition_table_test.go
// parses the fenced block and fails when this table and the diagram drift,
// so a Table change regenerates both in the same PR.
var diagramEdges = map[Edge]bool{
	{stagelabels.Actionable, stagelabels.Claimed}:                       true, // scanner / relay claim
	{stagelabels.Actionable, stagelabels.IssueNeedsDecision}:            true, // needs-human / park label (agent escalates)
	{stagelabels.Actionable, stagelabels.TriageNeeded}:                  true, // needs-triage added, or a hive-filed issue staged 010 before 015 existed (reclassification)
	{stagelabels.TriageNeeded, stagelabels.Actionable}:                  true, // maintainer triages: triage/accepted, hive/verified-open or approved-direction added, or needs-triage removed (not a regression)
	{stagelabels.TriageNeeded, stagelabels.Claimed}:                     true, // scanner / relay claim (015 is not a gate)
	{stagelabels.TriageNeeded, stagelabels.IssueNeedsDecision}:          true, // needs-human / park label (agent escalates)
	{stagelabels.Claimed, stagelabels.IssueNeedsDecision}:               true, // claim lapses on a parked issue
	{stagelabels.IssueNeedsDecision, stagelabels.Actionable}:            true, // /hive approve or /hive decision
	{stagelabels.IssueNeedsDecision, stagelabels.TriageNeeded}:          true, // park label removed by hand on an untriaged issue (approved-direction itself counts as triage)
	{stagelabels.Claimed, stagelabels.PRInProgress}:                     true, // PR under way (hive-open-pr request or start marker naming a branch)
	{stagelabels.Claimed, stagelabels.CoveredByPR}:                      true, // PR opened straight from the claim
	{stagelabels.Claimed, stagelabels.Actionable}:                       true, // claim released or lapsed with no PR
	{stagelabels.Claimed, stagelabels.TriageNeeded}:                     true, // claim released or lapsed with no PR on an untriaged issue
	{stagelabels.PRInProgress, stagelabels.CoveredByPR}:                 true, // PR opened
	{stagelabels.PRInProgress, stagelabels.Actionable}:                  true, // PR request failed / start mark lapsed with no PR
	{stagelabels.PRInProgress, stagelabels.TriageNeeded}:                true, // PR request failed / start mark lapsed with no PR on an untriaged issue
	{stagelabels.PRInProgress, stagelabels.Claimed}:                     true, // PR evidence lapsed while the claim is still held (backward hop, commented)
	{stagelabels.CoveredByPR, stagelabels.Actionable}:                   true, // covering PR closed unmerged with no live claim or start marker (backward hop, narrated on the PR)
	{stagelabels.CoveredByPR, stagelabels.TriageNeeded}:                 true, // covering PR closed unmerged on an untriaged issue (backward hop, narrated on the PR)
	{stagelabels.CoveredByPR, stagelabels.PRInProgress}:                 true, // the agent is back on the covering PR (live claim with the PR under way; not a regression)
	{stagelabels.CoveredByPR, stagelabels.Claimed}:                      true, // covering PR closed while the claim is still held (backward hop, narrated on the PR)
	{stagelabels.CoveredByPR, stagelabels.ReviewPending}:                true, // the PR takes over (issue stays 040; cross-kind)
	{stagelabels.ReviewPending, stagelabels.Reviewed}:                   true, // swarm approve
	{stagelabels.Reviewed, stagelabels.AwaitingMergeAdmission}:          true, // CI green, author side done
	{stagelabels.AwaitingMergeAdmission, stagelabels.PreMergeSoak}:      true, // lane admits, soak window running
	{stagelabels.AwaitingMergeAdmission, stagelabels.MergeQueue}:        true, // lane admits, no soak / soak already complete
	{stagelabels.PreMergeSoak, stagelabels.MergeQueue}:                  true, // soak complete
	{stagelabels.MergeQueue, stagelabels.PreMergeSoak}:                  true, // pre-095 record reclassified while still soaking (not a regression)
	{stagelabels.ReviewPending, stagelabels.Fixing}:                     true, // hive PR red / changes requested
	{stagelabels.ReviewPending, stagelabels.NeedsAuthor}:                true, // human PR red / changes requested
	{stagelabels.ReviewPending, stagelabels.ReturnedToContributor}:      true, // contributor PR red / changes requested
	{stagelabels.ReviewPending, stagelabels.NeedsDecision}:              true, // swarm requires_human; hold / sentinel / human-only rule
	{stagelabels.Reviewed, stagelabels.Fixing}:                          true, // new head red
	{stagelabels.PreMergeSoak, stagelabels.Fixing}:                      true, // new head red during the soak (backward hop, commented)
	{stagelabels.PreMergeSoak, stagelabels.ReturnedToContributor}:       true, // contributor head red during the soak (backward hop, commented)
	{stagelabels.MergeQueue, stagelabels.Fixing}:                        true, // new head red (backward hop, commented)
	{stagelabels.MergeQueue, stagelabels.ReturnedToContributor}:         true, // contributor head red (backward hop, commented)
	{stagelabels.Fixing, stagelabels.ReviewPending}:                     true, // fixed head pushed
	{stagelabels.Fixing, stagelabels.NeedsDecision}:                     true, // hive adoption copy still conflicting after two fixer rebase attempts (rebase-failed-twice)
	{stagelabels.NeedsAuthor, stagelabels.ReviewPending}:                true, // author pushes a new head
	{stagelabels.ReturnedToContributor, stagelabels.ReviewPending}:      true, // author pushes a new head
	{stagelabels.ReturnedToContributor, stagelabels.AdoptedByHive}:      true, // clanker unavailable, adopted
	{stagelabels.AdoptedByHive, stagelabels.AwaitingMergeAdmission}:     true, // in-place fix green and approved
	{stagelabels.AdoptedByHive, stagelabels.PreMergeSoak}:               true, // lane admits the hive's head, soak running
	{stagelabels.AdoptedByHive, stagelabels.MergeQueue}:                 true, // lane admits the hive's head, soak complete
	{stagelabels.AdoptedByHive, stagelabels.ReturnedToContributor}:      true, // superseder closed unmerged / contributor pushed over
	{stagelabels.AdoptedByHive, stagelabels.NeedsDecision}:              true, // adoption-stalled:<reason> / re-home failed
	{stagelabels.NeedsDecision, stagelabels.AdoptedByHive}:              true, // adoption-stalled reason cleared; escalation released
	{stagelabels.Reviewed, stagelabels.ReadyForHumanMerge}:              true, // untrusted tier, head otherwise mergeable
	{stagelabels.NeedsDecision, stagelabels.ReadyForHumanMerge}:         true, // hold lifted / alert cleared
	{stagelabels.ReadyForHumanMerge, stagelabels.NeedsDecision}:         true, // hold or sentinel alert added
	{stagelabels.ReadyForHumanMerge, stagelabels.ReviewPending}:         true, // new head pushed (verdict pending)
	{stagelabels.ReadyForHumanMerge, stagelabels.ReturnedToContributor}: true, // head red / DCO missing / conflicts
	{stagelabels.NeedsDecision, stagelabels.ReturnedToContributor}:      true, // head red / DCO missing / conflicts
	{stagelabels.NeedsDecision, stagelabels.Fixing}:                     true, // hive PR, trusted human changes_requested review or /hive decision (author agent kicked)
}
