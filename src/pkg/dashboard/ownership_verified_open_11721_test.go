package dashboard

import (
	"testing"
	"time"
)

func TestOwnershipVerifiedOpenCancelsLikelyDone11721(t *testing.T) {
	env := ownershipEnv11565(4)
	closeDone := want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone}
	stale := ownershipNow11565.AddDate(0, 0, -20)

	claimed := func(labels ...string) issueOwnershipInput {
		// A live agent claim withholds the issue from the actionable list.
		in := nonActionable11565("", "", labels...)
		in.liveClaim = true
		return in
	}
	gated := func(updated time.Time, labels ...string) issueOwnershipInput {
		in := freshIssue11565(labels...)
		in.lane = "scanner"
		in.reporterGate = true
		in.createdAt, in.updatedAt = updated, updated
		return in
	}

	tests := []struct {
		name string
		in   issueOwnershipInput
		want want11565
	}{
		{"likely-done alone, not actionable closes", claimed("hive/likely-done"), closeDone},
		{"likely-done + verified-open, not actionable", claimed("hive/likely-done", "hive/verified-open"), notYours11565},
		{"already-done + verified-open still closes", claimed("hive/already-done", "hive/verified-open"), closeDone},
		{"covered-by-pr + verified-open still closes", claimed("hive/covered-by-pr", "hive/verified-open"), closeDone},
		{"reporter gate, likely-done alone, fresh waits", gated(ownershipNow11565.Add(-time.Hour), "hive/likely-done"), notYours11565},
		{"reporter gate, likely-done alone, stale closes", gated(stale, "hive/likely-done"), want11565{OwnershipVerbClose, OwnershipReasonReporterConfirmationStale}},
		{"reporter gate, likely-done + verified-open, fresh", gated(ownershipNow11565.Add(-time.Hour), "hive/likely-done", "hive/verified-open"), notYours11565},
		{"reporter gate, likely-done + verified-open, stale", gated(stale, "hive/likely-done", "hive/verified-open"), notYours11565},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkOwnership11565(t, decideIssueOwnership(tt.in, env), tt.want)
		})
	}

	// Reporter-gate wait is the reporter's turn only without verified-open.
	if got := decideIssueOwnership(gated(ownershipNow11565.Add(-time.Hour), "hive/likely-done"), env); !got.WaitingElsewhere {
		t.Errorf("likely-done under the reporter gate should wait elsewhere, got %+v", got)
	}
	if got := decideIssueOwnership(gated(ownershipNow11565.Add(-time.Hour), "hive/likely-done", "hive/verified-open"), env); got.WaitingElsewhere {
		t.Errorf("verified-open must cancel the reporter-confirmation wait, got %+v", got)
	}
}
