package github

import (
	"testing"
	"time"
)

// hivecommons/hive#15559: Karibhou's PR #15561 ("Refs #15559") opened at
// 23:15:41Z and the webhook recorded its claim at 23:15:44Z. The eval cycle's
// claim scan had begun listing before the PR existed and reconciled
// authoritatively at 23:17:09Z, replacing the map without the claim. The issue
// went back to actionable and the scanner opened #15567 for it 14 minutes
// later while #15561 sat in review.

func webhookOpenClaim(observed time.Time) IssueClaim {
	return IssueClaim{
		Repo: "hive", Issue: 15559,
		PRNumber: 15561, PRRepo: "hive",
		PRURL:    "https://github.com/hivecommons/hive/pull/15561",
		PRAuthor: "Karibhou", PRState: PRStateOpen,
		ExternalAuthor: true, Reference: true,
		ObservedAt: observed, FirstObservedAt: observed,
	}
}

// TestReconcileScan_CarriesClaimRecordedAfterListingBegan pins the fix: a
// claim the webhook recorded after the scan started survives an authoritative
// reconcile that did not list its PR.
func TestReconcileScan_CarriesClaimRecordedAfterListingBegan(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 17, 9, 0, time.UTC)
	l := settledLedger(t, &now)
	scanStart := now.Add(-3 * time.Minute)
	claim := webhookOpenClaim(scanStart.Add(2 * time.Minute))
	if err := l.Record(claim); err != nil {
		t.Fatal(err)
	}

	l.ReconcileScan(ClaimScan{StartedAt: scanStart}, true, nil)

	got, ok := l.Lookup(claim.Repo, claim.Issue)
	if !ok {
		t.Fatal("claim recorded after the scan began must survive the authoritative reconcile")
	}
	if got.PRNumber != claim.PRNumber || !got.ObservedAt.Equal(claim.ObservedAt) {
		t.Fatalf("carried claim changed: %+v", got)
	}
}

// TestReconcileScan_DropsClaimOlderThanListing: a claim the listing should
// have contained and did not is still dropped — the PR closed.
func TestReconcileScan_DropsClaimOlderThanListing(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 17, 9, 0, time.UTC)
	l := settledLedger(t, &now)
	scanStart := now.Add(-3 * time.Minute)
	if err := l.Record(webhookOpenClaim(scanStart.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}

	l.ReconcileScan(ClaimScan{StartedAt: scanStart}, true, nil)

	if _, ok := l.Lookup("hive", 15559); ok {
		t.Fatal("claim observed before the listing began must be dropped when the scan no longer lists its PR")
	}
}

// TestReconcileScan_LiveStrongerClaimStillWins: the carry-forward goes through
// the rank rule, so a hive-authored closing claim the scan DID list for the
// same issue is not displaced by the carried weak one.
func TestReconcileScan_LiveStrongerClaimStillWins(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 17, 9, 0, time.UTC)
	l := settledLedger(t, &now)
	scanStart := now.Add(-3 * time.Minute)
	if err := l.Record(webhookOpenClaim(scanStart.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	strong := IssueClaim{
		Repo: "hive", Issue: 15559, PRNumber: 15567, PRRepo: "hive",
		PRAuthor: "hivecommons-hive[bot]", PRState: PRStateOpen, Closing: true,
		ObservedAt: scanStart, FirstObservedAt: scanStart,
	}

	l.ReconcileScan(ClaimScan{Claims: []IssueClaim{strong}, StartedAt: scanStart}, true, nil)

	got, ok := l.Lookup("hive", 15559)
	if !ok || got.PRNumber != strong.PRNumber {
		t.Fatalf("live stronger claim must win, got %+v", got)
	}
}

// TestReconcileScan_ZeroStartCarriesNothing: a scan with no StartedAt (the
// fail-closed path reuses the persisted ledger; legacy callers) behaves as
// ReconcileOpen always did.
func TestReconcileScan_ZeroStartCarriesNothing(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 17, 9, 0, time.UTC)
	l := settledLedger(t, &now)
	if err := l.Record(webhookOpenClaim(now)); err != nil {
		t.Fatal(err)
	}

	l.ReconcileScan(ClaimScan{}, true, nil)

	if _, ok := l.Lookup("hive", 15559); ok {
		t.Fatal("without a listing start the authoritative reconcile must replace the map as before")
	}
}
