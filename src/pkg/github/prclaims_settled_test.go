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

// hivecommons/hive#11580: settledClaimRetention was a calendar clock from the
// merge, so a fixed issue that nobody closed was re-offered on day 31 — the
// same every-N-days cycle #8003 fixed at 72h, just slower. The clock now
// restarts every pass the enumeration still lists the issue as open; only the
// issue leaving the open set starts the countdown.

func openSet(claims ...IssueClaim) OpenIssueSet {
	open := OpenIssueSet{}
	for _, c := range claims {
		open[c.Key()] = struct{}{}
	}
	return open
}

// TestSettledClaim_HeldWhileIssueStaysOpen: the issue is listed open on every
// pass, so the claim must still hold well past settledClaimRetention from the
// merge. Fails on the parent commit (the claim retires at merge+30d).
func TestSettledClaim_HeldWhileIssueStaysOpen(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	claim := mergedStrongClaim(now)
	open := openSet(claim)
	l.ReconcileOpen([]IssueClaim{claim}, true, open)

	// One pass a week for twelve weeks: the PR fell out of the scan after the
	// first, the issue is open throughout, the claim never lapses.
	for week := 1; week <= 12; week++ {
		now = now.Add(7 * 24 * time.Hour)
		l.ReconcileOpen(nil, true, open)
		got, ok := l.Lookup(claim.Repo, claim.Issue)
		if !ok {
			t.Fatalf("week %d: settled claim on a still-open issue must be carried (#11580)", week)
		}
		if !got.MergedAt.Equal(claim.MergedAt) {
			t.Fatalf("week %d: MergedAt must not move (it anchors SettledStale): %v", week, got.MergedAt)
		}
		if !got.IssueOpenAt.Equal(now) {
			t.Fatalf("week %d: IssueOpenAt must record this pass, got %v want %v", week, got.IssueOpenAt, now)
		}
		if !got.SettledStale(now) {
			t.Fatalf("week %d: a long-held settled claim must still read as stale so the queue asks a human to close", week)
		}
	}
}

// TestSettledClaim_RetiresAfterIssueCloses: once the issue leaves the open
// set, retention counts from the LAST open sighting, not from the merge.
func TestSettledClaim_RetiresAfterIssueCloses(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	claim := mergedStrongClaim(now)
	open := openSet(claim)
	l.ReconcileOpen([]IssueClaim{claim}, true, open)

	// Open for 45 days — already past merge+retention.
	now = now.Add(45 * 24 * time.Hour)
	l.ReconcileOpen(nil, true, open)
	lastOpen := now
	if _, ok := l.Lookup(claim.Repo, claim.Issue); !ok {
		t.Fatal("claim must be held while the issue is open")
	}

	// The issue closes: it stops appearing in the open set.
	now = lastOpen.Add(settledClaimRetention - time.Hour)
	l.ReconcileOpen(nil, true, nil)
	got, ok := l.Lookup(claim.Repo, claim.Issue)
	if !ok {
		t.Fatal("inside retention from the last open sighting the claim must still be carried")
	}
	if !got.IssueOpenAt.Equal(lastOpen) {
		t.Fatalf("IssueOpenAt must freeze at the last open sighting, got %v want %v", got.IssueOpenAt, lastOpen)
	}
	now = lastOpen.Add(settledClaimRetention + time.Hour)
	l.ReconcileOpen(nil, true, nil)
	if _, ok := l.Lookup(claim.Repo, claim.Issue); ok {
		t.Fatal("past retention from the last open sighting the claim must retire (ledger growth bound)")
	}
}

// TestSettledClaim_OpenAnchorHoldsThroughOutageAndRestart: the open-sighting
// anchor is persisted and honoured by the non-authoritative prune, so neither
// an API outage nor a pod restart re-offers an open issue whose fix merged
// more than settledClaimRetention ago.
func TestSettledClaim_OpenAnchorHoldsThroughOutageAndRestart(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "l.json")
	l := NewClaimLedger(path, testLogger())
	l.SetClock(func() time.Time { return now })
	claim := mergedStrongClaim(now)
	open := openSet(claim)
	l.ReconcileOpen([]IssueClaim{claim}, true, open)

	now = now.Add(settledClaimRetention + 24*time.Hour)
	l.ReconcileOpen(nil, false, open) // API down; issue still listed open
	if _, ok := l.Lookup(claim.Repo, claim.Issue); !ok {
		t.Fatal("non-authoritative prune must honour the open-sighting anchor")
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	// Load with the same frozen clock: the running hub kept this claim on
	// its open-sighting anchor, so a restart at the same instant must too.
	reloaded, err := loadClaimLedgerAt(path, testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Lookup(claim.Repo, claim.Issue)
	if !ok || !got.IssueOpenAt.Equal(now) {
		t.Fatalf("IssueOpenAt must survive Save/Load, got %+v", got)
	}
	if !got.ObservedAt.Equal(now) {
		t.Fatalf("non-authoritative retention must refresh ObservedAt so the TTL agrees, got %v", got.ObservedAt)
	}
	now = now.Add(time.Hour)
	reloaded.ReconcileOpen(nil, true, nil)
	if _, ok := reloaded.Lookup(claim.Repo, claim.Issue); !ok {
		t.Fatal("after restart the anchor must still carry the claim inside retention")
	}
}

// TestSettledClaim_OpenSetDoesNotPinUnsettledClaims: an open issue does not
// keep an UNSETTLED claim alive — an open-PR claim whose PR vanished from the
// scan is released exactly as before, and a merged weak claim is still not
// carried. Only settled evidence takes the anchor.
func TestSettledClaim_OpenSetDoesNotPinUnsettledClaims(t *testing.T) {
	now := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	openPR := IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 8, PRRepo: "o/r", ObservedAt: now, FirstObservedAt: now}
	mergedRef := IssueClaim{Repo: "o/r", Issue: 9, PRNumber: 10, PRRepo: "o/r", Reference: true, MergedPR: true, MergedAt: now, ObservedAt: now, FirstObservedAt: now}
	open := openSet(openPR, mergedRef)
	l.ReconcileOpen([]IssueClaim{openPR, mergedRef}, true, open)
	for _, c := range []IssueClaim{openPR, mergedRef} {
		if got, _ := l.Lookup(c.Repo, c.Issue); !got.IssueOpenAt.IsZero() {
			t.Fatalf("unsettled claim must not take the open anchor: %+v", got)
		}
	}

	now = now.Add(time.Hour)
	l.ReconcileOpen(nil, true, open)
	if _, ok := l.Lookup(openPR.Repo, openPR.Issue); ok {
		t.Fatal("an open-PR claim absent from an authoritative scan must still be released")
	}
	if _, ok := l.Lookup(mergedRef.Repo, mergedRef.Issue); ok {
		t.Fatal("a merged weak claim must still not be carried")
	}
}

// TestOpenIssuesOf: the set is keyed exactly like the ledger, and a nil
// result is an empty set rather than a panic.
func TestOpenIssuesOf(t *testing.T) {
	if got := OpenIssuesOf(nil); len(got) != 0 {
		t.Fatalf("nil result must yield an empty set, got %v", got)
	}
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{
		{Repo: "projectbluefin/chairlift", Number: 55},
		{Repo: "", Number: 1},
		{Repo: "o/r", Number: 0},
	})}
	got := OpenIssuesOf(result)
	if len(got) != 1 {
		t.Fatalf("want exactly the one well-formed issue, got %v", got)
	}
	if _, ok := got[mergedStrongClaim(time.Time{}).Key()]; !ok {
		t.Fatalf("set must be keyed like the ledger (repo#number), got %v", got)
	}
}

// TestLoadClaimLedger_SettledClaimOutlivesTTL: on disk a settled claim is
// kept by its retention bound, not the raw TTL (#8003, #11580) — a hub that
// restarts after a long outage must come back with the same claims the
// running hub would have kept, while an unsettled claim past the TTL is
// still dropped on load exactly as before.
func TestLoadClaimLedger_SettledClaimOutlivesTTL(t *testing.T) {
	saved := time.Date(2026, 9, 17, 4, 24, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "l.json")
	l := NewClaimLedger(path, testLogger())
	l.SetClock(func() time.Time { return saved })
	settled := mergedStrongClaim(saved)
	unsettled := IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 8, PRRepo: "o/r", ObservedAt: saved, FirstObservedAt: saved}
	l.ReconcileOpen([]IssueClaim{settled, unsettled}, true, nil)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	later := saved.Add(claimLedgerTTL + 24*time.Hour) // past the TTL, inside retention
	reloaded, err := loadClaimLedgerAt(path, testLogger(), func() time.Time { return later })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup(settled.Repo, settled.Issue); !ok {
		t.Fatal("a settled claim inside retention must survive load past the TTL")
	}
	if _, ok := reloaded.Lookup(unsettled.Repo, unsettled.Issue); ok {
		t.Fatal("an unsettled claim past the TTL must still be dropped on load")
	}

	expired := saved.Add(settledClaimRetention + time.Hour)
	reloaded, err = loadClaimLedgerAt(path, testLogger(), func() time.Time { return expired })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup(settled.Repo, settled.Issue); ok {
		t.Fatal("a settled claim past retention must be dropped on load")
	}
}
