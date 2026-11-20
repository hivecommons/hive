package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// recordRebaseAttemptDeliveries starts the deadline of every routed fixer
// rebase attempt that a kick delivered to agent at at carried.
//
// writeMergeEligible routes a fixer-owned rebase (adopted, trusted-author,
// operator-requested) by listing it in ci-failing.json; nothing is sent to
// the fixer until its next kick that renders the fix-before-new section.
// The attempt's deadline used to run from the routing pass, so a fixer that
// was not kicked in time (its cadence slot not due yet, or the slot taken by
// a single-task review-fix kick that carries no fix-before-new section) had
// both attempts expire without ever seeing the work, and an adopted fork PR
// parked at 085 adoption-stalled:fork-rehome-refused. Only a kick that
// actually reached the agent with the row in it counts.
func recordRebaseAttemptDeliveries(agent, message string, at time.Time, logger *slog.Logger) {
	if message == "" {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, a := range rebaseAttempts().AwaitingDelivery() {
		if !scheduler.KickCarriesRebaseAttempt(message, a.Repo, a.Number, a.Count()) {
			continue
		}
		d, first := rebaseAttempts().Deliver(a.Repo, a.Number, a.Count(), at)
		if !first {
			continue
		}
		logger.Info("rebase attempt delivered to fixer",
			"repo", d.Repo, "number", d.Number, "attempt", d.Count(), "max", rebasemerge.MaxRebaseAttempts,
			"fixer", agent, "routed_to", d.Fixer, "queued_for", d.Waiting(at).Round(time.Second).String())
	}
}
