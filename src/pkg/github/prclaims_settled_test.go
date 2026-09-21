package github

import (
	"path/filepath"
	"testing"
	"time"
)

// hivecommons/hive#8003: a merged strong claim on an open issue used to expire
// after 72h on a clock — the merged-PR settle scan looks back
// mergedClaimScanWindow, the ledger TTL is the same 72h, and once the PR fell
// out of the scan the authoritative Reconcile replaced the map without it. The
// issue went back into the offer pool every three days for as long as nobody
// closed it (chairlift#55: fixed by #133 on 09-17, re-offered 09-20, 09-21…).
// These tests pin the fix: settled claims are carried forward past the scan
// window, retire only at settledClaimRetention, survive the non-authoritative
// prune, and report SettledStale once a human should be asked to close.

func settledLedger(t *testing.T, now *time.Time) *ClaimLedger {
	t.Helper()
	l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	l.SetClock(func() time.Time { return *now })
	return l
}

func mergedStrongClaim(now time.Time) IssueClaim {
	return IssueClaim{
		Repo: "projectbluefin/chairlift", Issue: 55,
		PRNumber: 133, PRRepo: "projectbluefin/chairlift",
		PRURL:    "https://github.com/projectbluefin/chairlift/pull/133",
		PRAuthor: "mendezr", ExternalAuthor: false,
		MergedPR: true, MergedAt: now,
		ObservedAt: now, FirstObservedAt: now,
	}
}

// TestSettledClaim_MergedStrongClaimOutlivesTheScanWindow: the exact #8003
// shape. The scan finds #133 while it is inside mergedClaimScanWindow, then
// stops listing it; the claim must still suppress chairlift#55.
func TestSettledClaim_MergedStrongClaimOutlivesTheScanWindow(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	claim := mergedStrongClaim(now)

	l.Reconcile([]IssueClaim{claim}, true)
	if _, ok := l.Lookup(claim.Repo, claim.Issue); !ok {
		t.Fatal("scan-found merged claim must be recorded")
	}

	// 09-20 04:25Z — the PR is now older than mergedClaimScanWindow, so the
	// scan returns nothing for it. Before #8003 this reconcile dropped it.
	now = now.Add(mergedClaimScanWindow + time.Minute)
	l.Reconcile(nil, true)
	got, ok := l.Lookup(claim.Repo, claim.Issue)
	if !ok {
		t.Fatal("a merged strong claim must be carried past the scan window — the PR is still merged (#8003)")
	}
	if got.PRNumber != 133 || !got.MergedPR {
		t.Fatalf("carried claim lost its identity: %+v", got)
	}
	if !got.MergedAt.Equal(claim.MergedAt) {
		t.Fatalf("MergedAt must not be refreshed on carry (it anchors retention and staleness): %v", got.MergedAt)
	}

	// Two more cycles across another week: still held.
	for i := 0; i < 2; i++ {
		now = now.Add(mergedClaimScanWindow + time.Hour)
		l.Reconcile(nil, true)
		if _, ok := l.Lookup(claim.Repo, claim.Issue); !ok {
			t.Fatalf("carried claim dropped on cycle %d", i)
		}
	}
}

// TestSettledClaim_RetiresAtRetentionNotTTL: the only clock that ends a
// settled claim is settledClaimRetention from the merge.
func TestSettledClaim_RetiresAtRetentionNotTTL(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	claim := mergedStrongClaim(now)
	l.Reconcile([]IssueClaim{claim}, true)

	now = claim.MergedAt.Add(settledClaimRetention - time.Minute)
	l.Reconcile(nil, true)
	if _, ok := l.Lookup(claim.Repo, claim.Issue); !ok {
		t.Fatal("inside retention the claim must be carried")
	}
	now = claim.MergedAt.Add(settledClaimRetention + time.Minute)
	l.Reconcile(nil, true)
	if _, ok := l.Lookup(claim.Repo, claim.Issue); ok {
		t.Fatal("past retention the claim must retire (ledger growth bound for issues that did close)")
	}
}

// TestSettledClaim_SurvivesNonAuthoritativePrune: an API outage longer than
// the 72h TTL must not retire a settled claim — its evidence is not the API's
// reachability. An unsettled claim still prunes as before.
func TestSettledClaim_SurvivesNonAuthoritativePrune(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	settled := mergedStrongClaim(now)
	open := IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 8, PRRepo: "o/r", ObservedAt: now, FirstObservedAt: now}
	l.Reconcile([]IssueClaim{settled, open}, true)

	now = now.Add(claimLedgerTTL + time.Hour)
	l.Reconcile(nil, false)
	if _, ok := l.Lookup(settled.Repo, settled.Issue); !ok {
		t.Fatal("settled claim must survive the non-authoritative prune inside retention")
	}
	if _, ok := l.Lookup(open.Repo, open.Issue); ok {
		t.Fatal("an unsettled claim past the TTL must still prune on the non-authoritative path")
	}
}

// TestSettledClaim_MergedWeakClaimIsNotCarried: a merged `Refs #N` or an
// external author's merged PR never asserted it closed the issue.
// FilterClaimedIssues releases those with context (#7061); carrying them
// would park the remainder behind evidence that does not claim to finish it.
func TestSettledClaim_MergedWeakClaimIsNotCarried(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	ref := mergedStrongClaim(now)
	ref.Issue = 56
	ref.Reference = true
	ext := mergedStrongClaim(now)
	ext.Issue = 57
	ext.ExternalAuthor = true
	if ref.Settled() || ext.Settled() {
		t.Fatal("merged weak claims are not settled")
	}
	l.Reconcile([]IssueClaim{ref, ext}, true)

	now = now.Add(mergedClaimScanWindow + time.Minute)
	l.Reconcile(nil, true)
	if _, ok := l.Lookup(ref.Repo, 56); ok {
		t.Fatal("a merged reference claim must not be carried past the scan window")
	}
	if _, ok := l.Lookup(ext.Repo, 57); ok {
		t.Fatal("a merged external-author claim must not be carried past the scan window")
	}
}

// TestSettledClaim_LiveOpenClaimStillOutranksCarried: the #6867 rank rule is
// untouched — a new open strong PR for the same issue replaces the carried
// merged claim, and once it closes the carried claim does not resurrect.
func TestSettledClaim_LiveOpenClaimStillOutranksCarried(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	claim := mergedStrongClaim(now)
	l.Reconcile([]IssueClaim{claim}, true)

	now = now.Add(mergedClaimScanWindow + time.Minute)
	live := IssueClaim{Repo: claim.Repo, Issue: claim.Issue, PRNumber: 900, PRRepo: claim.Repo, ObservedAt: now}
	l.Reconcile([]IssueClaim{live}, true)
	if got, _ := l.Lookup(claim.Repo, claim.Issue); got.PRNumber != 900 {
		t.Fatalf("live open claim must win over the carried merged claim, got %+v", got)
	}
	l.Reconcile(nil, true)
	if _, ok := l.Lookup(claim.Repo, claim.Issue); ok {
		t.Fatal("displaced merged claim must not resurrect")
	}
}

// TestSettledClaim_Stale pins the human-nudge boundary: SettledStale is false
// until SettledClaimStaleAfter from the merge and true after; unsettled
// claims are never stale.
func TestSettledClaim_Stale(t *testing.T) {
	merged := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	claim := mergedStrongClaim(merged)
	if claim.SettledStale(merged.Add(SettledClaimStaleAfter - time.Minute)) {
		t.Fatal("not stale before SettledClaimStaleAfter")
	}
	if !claim.SettledStale(merged.Add(SettledClaimStaleAfter)) {
		t.Fatal("stale at SettledClaimStaleAfter")
	}
	verdict := IssueClaim{Repo: "o/r", Issue: 1, PRNumber: 2, PRRepo: "o/r", Source: ClaimSourceVerdict, ObservedAt: merged, FirstObservedAt: merged}
	if !verdict.SettledStale(merged.Add(SettledClaimStaleAfter)) {
		t.Fatal("a verdict claim anchors staleness at its first observation")
	}
	open := IssueClaim{Repo: "o/r", Issue: 3, PRNumber: 4, PRRepo: "o/r", ObservedAt: merged, FirstObservedAt: merged}
	if open.SettledStale(merged.Add(365 * 24 * time.Hour)) {
		t.Fatal("an open-PR claim is never settled-stale")
	}
}
