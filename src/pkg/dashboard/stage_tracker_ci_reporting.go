package dashboard

import "strings"

// A PR whose required check GitHub still reports unfinished after its job
// finished keeps its CI-pending stage (hivecommons/hive#15941), but the
// your-turn decision puts the rerun on a person (ownership_ci_reporting.go).
// The Activity monitor says the same thing: the row is Needs human with the
// decision's instruction as its needs_human_why reason, so it is in the
// Waiting on you list (stageTrackerFallbackWait) without a stage, modifier
// or caption of its own.

// needsHumanSourceCIReportingStalled names this wait as a needs_human_why
// source.
const needsHumanSourceCIReportingStalled = "ci-reporting-stalled"

// stageTrackerSetCIReportingStalled copies the stored decision's instruction
// onto a PR row, with the head the row was built for.
func stageTrackerSetCIReportingStalled(it *StageTrackerItem, decisions map[string]Ownership) {
	it.ciReportingStalled, it.ciReportingStalledHead = "", ""
	if it.Kind != "pr" {
		return
	}
	o := ownershipFor(decisions, ownershipKindPR, it.Number)
	if o == nil || !o.YourTurn || o.ReasonCode != OwnershipReasonCIReportingStalled || strings.TrimSpace(o.Reason) == "" {
		return
	}
	it.ciReportingStalled, it.ciReportingStalledHead = strings.TrimSpace(o.Reason), strings.TrimSpace(it.Head)
}

// stageTrackerCIReportingStalledReason is the row's stall instruction, or ""
// when it has none or a projection moved the row to another head since the
// decision was made: the stall was on that head only.
func stageTrackerCIReportingStalledReason(it StageTrackerItem) string {
	if it.ciReportingStalled == "" || it.ciReportingStalledHead == "" || !strings.EqualFold(it.ciReportingStalledHead, strings.TrimSpace(it.Head)) {
		return ""
	}
	return it.ciReportingStalled
}
