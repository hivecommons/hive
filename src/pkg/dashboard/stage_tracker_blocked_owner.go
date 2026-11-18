package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// A `blocked` issue whose blocker is a person's action (a permission to
// grant, an account to set up, work that lives in another repository) is not
// Actionable: no agent can move it. The blocked-on marker's kind=owner-action
// form says so, and the enumeration carries it as the filtered reason
// "blocked owner-action: <reason>". The row then reads Needs human with the
// reason as its caption line.

// needsHumanSourceBlockedOnOwner names this wait as a needs_human_why source.
// The reason line is stageTrackerBlockedOwnerLine.
const needsHumanSourceBlockedOnOwner = "blocked-on-owner"

// stageTrackerBlockedOwnerChip is the one phrase for a wait on a person.
const stageTrackerBlockedOwnerChip = "Needs human"

// stageTrackerBlockedOwnerPrefix opens the reason line.
const stageTrackerBlockedOwnerPrefix = "needs an owner action"

// stageTrackerBlockedOwnerLine is the one-line reason: "needs an owner
// action: <reason>", or the bare phrase when the marker carries no reason.
func stageTrackerBlockedOwnerLine(reason string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return stageTrackerBlockedOwnerPrefix + ": " + r
	}
	return stageTrackerBlockedOwnerPrefix
}

// stageTrackerBlockedOwner fills the row's BlockedOwnerReason and caption
// from an owner-action blocked reason; it reports whether the reason was one.
// Like the PR variant it never adds the `blocked` modifier (the label does).
func stageTrackerBlockedOwner(it *StageTrackerItem, filterReason string) bool {
	reason, ok := github.ParseBlockedOwnerReason(filterReason)
	if !ok {
		return false
	}
	line := stageTrackerBlockedOwnerLine(reason)
	it.BlockedOwnerReason = line
	clause := stageTrackerBlockedOwnerChip + ": " + line
	if it.Stage == 0 {
		it.Caption = clause
		it.Actor = "owner"
		return true
	}
	it.Caption = joinClause(it.Caption, clause)
	return true
}
