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
	{stagelabels.Claimed, stagelabels.IssueNeedsDecision}:               true, // claim lapses on a parked issue
	{stagelabels.IssueNeedsDecision, stagelabels.Actionable}:            true, // /hive approve or /hive decision
	{stagelabels.Claimed, stagelabels.PRInProgress}:                     true, // kick running
	{stagelabels.PRInProgress, stagelabels.CoveredByPR}:                 true, // PR opened
	{stagelabels.CoveredByPR, stagelabels.ReviewPending}:                true, // the PR takes over (issue stays 040; cross-kind)
	{stagelabels.ReviewPending, stagelabels.Reviewed}:                   true, // swarm approve
	{stagelabels.Reviewed, stagelabels.AwaitingMergeAdmission}:          true, // CI green, author side done
	{stagelabels.AwaitingMergeAdmission, stagelabels.MergeQueue}:        true, // lane admits (soak / merge-eligible)
	{stagelabels.ReviewPending, stagelabels.Fixing}:                     true, // hive PR red / changes requested
	{stagelabels.ReviewPending, stagelabels.ReturnedToContributor}:      true, // contributor PR red / changes requested
	{stagelabels.ReviewPending, stagelabels.NeedsDecision}:              true, // swarm requires_human; hold / sentinel / human-only rule
	{stagelabels.Reviewed, stagelabels.Fixing}:                          true, // new head red
	{stagelabels.MergeQueue, stagelabels.Fixing}:                        true, // new head red (backward hop, commented)
	{stagelabels.MergeQueue, stagelabels.ReturnedToContributor}:         true, // contributor head red (backward hop, commented)
	{stagelabels.Fixing, stagelabels.ReviewPending}:                     true, // fixed head pushed
	{stagelabels.ReturnedToContributor, stagelabels.ReviewPending}:      true, // author pushes a new head
	{stagelabels.ReturnedToContributor, stagelabels.AdoptedByHive}:      true, // clanker unavailable, adopted
	{stagelabels.AdoptedByHive, stagelabels.AwaitingMergeAdmission}:     true, // in-place fix green and approved
	{stagelabels.AdoptedByHive, stagelabels.MergeQueue}:                 true, // lane admits the hive's head
	{stagelabels.AdoptedByHive, stagelabels.ReturnedToContributor}:      true, // superseder closed unmerged / contributor pushed over
	{stagelabels.AdoptedByHive, stagelabels.NeedsDecision}:              true, // adoption-stalled:<reason> / re-home failed
	{stagelabels.Reviewed, stagelabels.ReadyForHumanMerge}:              true, // untrusted tier, head otherwise mergeable
	{stagelabels.NeedsDecision, stagelabels.ReadyForHumanMerge}:         true, // hold lifted / alert cleared
	{stagelabels.ReadyForHumanMerge, stagelabels.NeedsDecision}:         true, // hold or sentinel alert added
	{stagelabels.ReadyForHumanMerge, stagelabels.ReviewPending}:         true, // new head pushed (verdict pending)
	{stagelabels.ReadyForHumanMerge, stagelabels.ReturnedToContributor}: true, // head red / DCO missing / conflicts
	{stagelabels.NeedsDecision, stagelabels.ReturnedToContributor}:      true, // head red / DCO missing / conflicts
}
