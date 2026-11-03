package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#9945: the issues.reopened webhook must void the merged
// claim and persist it, so the next pass treats the issue as unclaimed.
func TestIssueReopenedHookVoidsClaimAndPersists(t *testing.T) {
	path := resetClaimLedgerForTest(t)
	var buf strings.Builder
	logger := sweepTestLogger(&buf)
	ledger := getClaimLedger(logger)
	merged := time.Date(2026, 10, 14, 1, 46, 50, 0, time.UTC)
	ledger.Reconcile([]github.IssueClaim{{
		Repo: "hive", Issue: 9945, PRNumber: 11758, PRRepo: "hive", MergedPR: true, MergedAt: merged,
		ObservedAt: merged, FirstObservedAt: merged,
	}}, true)

	hook := issueReopenedHookFor(ledger, logger)
	hook("hive", 9945, merged.Add(-time.Hour))
	if _, ok := ledger.Lookup("hive", 9945); !ok {
		t.Fatal("reopen before merge must not void the claim")
	}
	hook("hive", 9945, merged.Add(90*time.Minute))
	if _, ok := ledger.Lookup("hive", 9945); ok {
		t.Fatal("reopen after merge must void the claim")
	}
	if !strings.Contains(buf.String(), "reopened issue re-queued") || !strings.Contains(buf.String(), "source=webhook") {
		t.Fatalf("missing re-queue log line: %s", buf.String())
	}
	reloaded, err := github.LoadClaimLedger(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup("hive", 9945); ok {
		t.Fatal("voided claim must be persisted")
	}

	// Nil ledger and unknown issues are no-ops.
	issueReopenedHookFor(nil, logger)("hive", 1, merged)
	hook("hive", 1, merged.Add(time.Hour))
}
