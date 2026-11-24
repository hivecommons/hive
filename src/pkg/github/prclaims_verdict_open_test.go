package github

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A no_work_needed verdict citing an OPEN PR that later closed unmerged was
// recorded as a weak external claim, but Settled() returned true
// for every verdict claim. So it was carried like a landed fix, outlived #544
// closing unmerged, and after 7 days was withheld as merged_claim_stale
// "citing merged PR #544". These tests pin the fix: an open-PR verdict claim
// is not settled; it covers the issue while the scan's open listing still
// shows the PR, and is released the first pass a complete listing does not.
// Merged-PR and commit verdict claims are unchanged.

const verdictOpenRepo = "octo/tools"

func openPRVerdictClaim(now time.Time) IssueClaim {
	return IssueClaim{
		Repo: verdictOpenRepo, Issue: 543,
		PRNumber: 544, PRRepo: verdictOpenRepo,
		PRURL:     "https://github.com/octo/tools/pull/544",
		PRAuthor:  "someone-else",
		PRState:   PRStateOpen,
		Reference: true, ExternalAuthor: true,
		Source: ClaimSourceVerdict, SourceReporter: "reporter",
		ObservedAt: now, FirstObservedAt: now,
	}
}

// completeListing is an open listing that ran to its last page for repo and
// returned exactly prs.
func completeListing(repo string, prs ...int) *OpenPRListing {
	owner, name := splitRepoName(repo)
	l := &OpenPRListing{}
	l.markComplete(owner, name)
	for _, n := range prs {
		l.addPR(owner, name, n)
	}
	return l
}

func verdictIssueResult(issue int) *ActionableResult {
	return &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: verdictOpenRepo, Number: issue, Title: "shared-ci"}}, Count: 1}}
}

func TestOpenVerdictClaim_CoversWhileOpenAndIsNeverSettled(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	c := openPRVerdictClaim(now)
	if c.Settled() {
		t.Fatal("a verdict citing an open PR has landed nothing; it must not be settled")
	}
	if !c.CoversOpenIssue() {
		t.Fatal("an open-PR verdict claim is a weak external claim and covers its issue")
	}
	if !c.SettledAt().IsZero() || c.SettledStale(now.Add(365*24*time.Hour)) {
		t.Fatal("an unsettled claim has no settled instant and is never stale")
	}

	l := settledLedger(t, &now)
	if err := l.Record(c); err != nil {
		t.Fatal(err)
	}
	open := openSet(c)
	// Five scans three days apart: past the 72h TTL and past
	// SettledClaimStaleAfter. Each listing still shows #544 open.
	for pass := 1; pass <= 5; pass++ {
		now = now.Add(3 * 24 * time.Hour)
		scan := ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo, 544, 700)}
		l.ReconcileScan(scan, true, open)
		got, ok := l.Lookup(verdictOpenRepo, 543)
		if !ok {
			t.Fatalf("pass %d: the claim must hold while its PR is listed open", pass)
		}
		if !got.ObservedAt.Equal(now) || got.PRState != PRStateOpen {
			t.Fatalf("pass %d: the listing confirms the PR open, so ObservedAt must refresh: %+v", pass, got)
		}
		if got.Settled() || got.SettledStale(now) || !got.IssueOpenAt.IsZero() {
			t.Fatalf("pass %d: the claim must stay an unsettled open-PR claim: %+v", pass, got)
		}
	}
	if got := FilterClaimedIssues(verdictIssueResult(543), l, nil, testLogger()); got != 1 {
		t.Fatalf("filter suppressed %d, want the issue covered by the open external PR", got)
	}

	// A restart keeps it: the last confirmation is fresh.
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadClaimLedgerAt(l.path, testLogger(), func() time.Time { return now.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup(verdictOpenRepo, 543); !ok {
		t.Fatal("a confirmed open-PR verdict claim must survive a restart")
	}
}

func TestOpenVerdictClaim_ReleasedWhenPRClosesUnmerged(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	if err := l.Record(openPRVerdictClaim(now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(26 * time.Hour)
	// #544 closed unmerged: the complete listing no longer returns it.
	l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo, 700)}, true, openSet(openPRVerdictClaim(now)))
	if got, ok := l.LookupAny(verdictOpenRepo, 543); ok {
		t.Fatalf("the claim must be released once its PR is not open: %+v", got)
	}
	result := verdictIssueResult(543)
	if got := FilterClaimedIssues(result, l, nil, testLogger()); got != 0 || len(result.Issues.Items) != 1 {
		t.Fatalf("filter suppressed %d, kept %+v; want the issue admissible again", got, result.Issues.Items)
	}
}

// TestOpenVerdictClaim_MergeReleasesUnlessTheScanCoversIt: a merge ends the
// open-PR claim. It is never turned into a merged verdict claim here, because
// verifySettlingPR deliberately refuses a PR merged after the task was
// dispatched. If the merged PR references the issue the scan's own merged
// claim covers it; if not, the issue is released and the next verdict decides.
func TestOpenVerdictClaim_MergeReleasesUnlessTheScanCoversIt(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

	t.Run("PR references the issue", func(t *testing.T) {
		now := now
		l := settledLedger(t, &now)
		if err := l.Record(openPRVerdictClaim(now)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		// The merged-PR settle scan produces its own strong merged claim.
		scanned := IssueClaim{
			Repo: verdictOpenRepo, Issue: 543, PRNumber: 544, PRRepo: verdictOpenRepo,
			Closing: true, MergedPR: true, MergedAt: now.Add(-time.Minute), PRState: PRStateMerged,
			ObservedAt: now, FirstObservedAt: now.Add(-time.Minute),
		}
		l.ReconcileScan(ClaimScan{Claims: []IssueClaim{scanned}, StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo)}, true, nil)
		got, ok := l.Lookup(verdictOpenRepo, 543)
		if !ok || got.Source != "" || !got.MergedPR || !got.Settled() {
			t.Fatalf("the scan's merged claim must take over unchanged, got %+v (found %v)", got, ok)
		}
	})

	t.Run("PR does not reference the issue", func(t *testing.T) {
		now := now
		l := settledLedger(t, &now)
		if err := l.Record(openPRVerdictClaim(now)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo)}, true, nil)
		if _, ok := l.LookupAny(verdictOpenRepo, 543); ok {
			t.Fatal("a merged PR the scan cannot attribute releases the open-PR claim; the next verdict decides")
		}
	})
}

// TestVerdictClaim_LandedShapesStaySettled: a verdict citing a merged PR, and
// one citing a reachable commit (verifySettlingCommit records PRNumber 0, the
// commit URL and MergedPR), are settled exactly as before: carried past an
// authoritative scan that cannot see them, and stale after 7 days.
func TestVerdictClaim_LandedShapesStaySettled(t *testing.T) {
	merged := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	now := merged
	l := settledLedger(t, &now)
	mergedPRClaim := IssueClaim{
		Repo: verdictOpenRepo, Issue: 10, PRNumber: 11, PRRepo: verdictOpenRepo,
		MergedPR: true, MergedAt: merged, Source: ClaimSourceVerdict,
		ObservedAt: merged, FirstObservedAt: merged,
	}
	commitClaim := IssueClaim{
		Repo: verdictOpenRepo, Issue: 12, PRRepo: verdictOpenRepo,
		PRURL:    "https://github.com/octo/tools/commit/abc1234bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		MergedPR: true, MergedAt: merged, Source: ClaimSourceVerdict,
		ObservedAt: merged, FirstObservedAt: merged,
	}
	for _, c := range []IssueClaim{mergedPRClaim, commitClaim} {
		if !c.Settled() || c.CoversOpenIssue() {
			t.Fatalf("a landed verdict claim is settled and not an open-PR cover: %+v", c)
		}
		if err := l.Record(c); err != nil {
			t.Fatal(err)
		}
	}
	open := openSet(mergedPRClaim, commitClaim)
	for pass := 1; pass <= 3; pass++ {
		now = now.Add(3 * 24 * time.Hour)
		// A complete listing that does not show #11 must not release a
		// merged claim: it is settled, not an open-PR claim.
		l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo, 700)}, true, open)
		for _, c := range []IssueClaim{mergedPRClaim, commitClaim} {
			if _, ok := l.Lookup(c.Repo, c.Issue); !ok {
				t.Fatalf("pass %d: settled verdict claim on #%d must be carried", pass, c.Issue)
			}
		}
	}
	for _, c := range []IssueClaim{mergedPRClaim, commitClaim} {
		if c.SettledStale(merged.Add(SettledClaimStaleAfter - time.Minute)) {
			t.Fatalf("#%d: not stale before SettledClaimStaleAfter", c.Issue)
		}
		if !c.SettledStale(merged.Add(SettledClaimStaleAfter)) {
			t.Fatalf("#%d: stale at SettledClaimStaleAfter", c.Issue)
		}
	}
}

// TestOpenVerdictClaim_StoredClaimFromBeforeTheFix: the ledger on disk holds
// open-PR verdict claims written while they counted as settled (ObservedAt
// refreshed every pass, an old FirstObservedAt, an IssueOpenAt anchor). No
// migration step: on load they are ordinary unsettled claims inside the TTL,
// and the next scan drops the one whose PR closed and keeps the one whose PR
// is still open.
func TestOpenVerdictClaim_StoredClaimFromBeforeTheFix(t *testing.T) {
	saved := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	old := NewClaimLedger(path, testLogger())
	old.SetClock(func() time.Time { return saved })
	closedPR := openPRVerdictClaim(saved)
	closedPR.FirstObservedAt = saved.Add(-20 * 24 * time.Hour)
	closedPR.IssueOpenAt = saved
	stillOpen := closedPR
	stillOpen.Issue, stillOpen.PRNumber = 545, 546
	for _, c := range []IssueClaim{closedPR, stillOpen} {
		if err := old.Record(c); err != nil {
			t.Fatal(err)
		}
	}

	now := saved.Add(time.Hour)
	l, err := loadClaimLedgerAt(path, testLogger(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	got, ok := l.Lookup(verdictOpenRepo, 543)
	if !ok {
		t.Fatal("a recently confirmed claim loads inside the TTL")
	}
	if got.Settled() || got.SettledStale(now) {
		t.Fatalf("a stored open-PR verdict claim is no longer settled or stale: %+v", got)
	}

	now = now.Add(10 * time.Minute)
	l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo, 546)}, true, openSet(closedPR, stillOpen))
	if _, ok := l.LookupAny(verdictOpenRepo, 543); ok {
		t.Fatal("the stored claim whose PR closed must be dropped on the next scan")
	}
	if got, ok := l.Lookup(verdictOpenRepo, 545); !ok || !got.ObservedAt.Equal(now) {
		t.Fatalf("the stored claim whose PR is still open must be kept and confirmed: %+v (found %v)", got, ok)
	}
}

// TestOpenVerdictClaim_UnknownListingFallsBackToTTL: when the scan cannot say
// (no listing, the PR's repo not listed completely) the claim is neither
// confirmed nor released; it lives on the TTL from its last confirmation. A
// claim verified after the listing began is newer than the listing, so the
// listing's silence does not release it (#15559).
func TestOpenVerdictClaim_UnknownListingFallsBackToTTL(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

	t.Run("repo not listed completely", func(t *testing.T) {
		now := start
		l := settledLedger(t, &now)
		if err := l.Record(openPRVerdictClaim(now)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(claimLedgerTTL - time.Hour)
		l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing("o/other", 1)}, true, nil)
		got, ok := l.Lookup(verdictOpenRepo, 543)
		if !ok || !got.ObservedAt.Equal(start) {
			t.Fatalf("an unknown listing keeps the claim unconfirmed inside the TTL: %+v (found %v)", got, ok)
		}
		now = start.Add(claimLedgerTTL + time.Hour)
		l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing("o/other", 1)}, true, nil)
		if _, ok := l.Lookup(verdictOpenRepo, 543); ok {
			t.Fatal("past the TTL an unconfirmed claim must be released")
		}
	})

	t.Run("no listing", func(t *testing.T) {
		now := start
		l := settledLedger(t, &now)
		if err := l.Record(openPRVerdictClaim(now)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		l.Reconcile(nil, true)
		if _, ok := l.Lookup(verdictOpenRepo, 543); !ok {
			t.Fatal("Reconcile without a listing keeps the claim inside the TTL")
		}
		now = start.Add(claimLedgerTTL + time.Hour)
		l.Reconcile(nil, true)
		if _, ok := l.Lookup(verdictOpenRepo, 543); ok {
			t.Fatal("and releases it past the TTL")
		}
	})

	t.Run("verified after the listing began", func(t *testing.T) {
		now := start
		l := settledLedger(t, &now)
		listedAt := now.Add(-time.Minute)
		if err := l.Record(openPRVerdictClaim(now)); err != nil {
			t.Fatal(err)
		}
		l.ReconcileScan(ClaimScan{StartedAt: listedAt, OpenPRs: completeListing(verdictOpenRepo, 700)}, true, nil)
		if _, ok := l.Lookup(verdictOpenRepo, 543); !ok {
			t.Fatal("a listing older than the verdict check must not release its claim")
		}
		l.ReconcileScan(ClaimScan{StartedAt: listedAt, OpenPRs: completeListing(verdictOpenRepo, 700)}, false, nil)
		if _, ok := l.Lookup(verdictOpenRepo, 543); !ok {
			t.Fatal("nor on the partial-scan path")
		}
	})
}

// TestOpenVerdictClaim_PartialScanUsesCompleteListings: a scan that failed in
// some other repo is not authoritative, but a repo it did list completely
// still tells the truth about its PRs.
func TestOpenVerdictClaim_PartialScanUsesCompleteListings(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	l := settledLedger(t, &now)
	if err := l.Record(openPRVerdictClaim(now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo, 544)}, false, nil)
	if got, ok := l.Lookup(verdictOpenRepo, 543); !ok || !got.ObservedAt.Equal(now) {
		t.Fatalf("a complete listing showing the PR open confirms the claim: %+v (found %v)", got, ok)
	}
	now = now.Add(time.Hour)
	l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: completeListing(verdictOpenRepo)}, false, nil)
	if _, ok := l.Lookup(verdictOpenRepo, 543); ok {
		t.Fatal("a complete listing without the PR releases the claim even on a partial scan")
	}
}

func TestOpenPRListing_Lists(t *testing.T) {
	l := completeListing("octo/tools", 544)
	for _, repo := range []string{"octo/tools", "Octo/Tools", "/octo/tools/"} {
		if open, known := l.Lists(repo, 544); !open || !known {
			t.Fatalf("Lists(%q, 544) = %v, %v; want open", repo, open, known)
		}
		if open, known := l.Lists(repo, 545); open || !known {
			t.Fatalf("Lists(%q, 545) = %v, %v; want known not open", repo, open, known)
		}
	}
	// A bare spelling is not answered by an owner-qualified listing: it
	// could be any org's repo of that name.
	if open, known := l.Lists("tools", 544); open || known {
		t.Fatalf("a bare spelling must be unknown to an owner-qualified listing, got %v, %v", open, known)
	}
	if open, known := l.Lists("o/unlisted", 1); open || known {
		t.Fatalf("an unlisted repo must be unknown, got %v, %v", open, known)
	}
	// A scan with no owner records bare keys, and only bare spellings match.
	bare := completeListing("tools", 7)
	if open, known := bare.Lists("tools", 7); !open || !known {
		t.Fatalf("bare listing, bare spelling = %v, %v; want open", open, known)
	}
	if open, known := bare.Lists("octo/tools", 7); open || known {
		t.Fatalf("bare listing, owner spelling = %v, %v; want unknown", open, known)
	}
	var none *OpenPRListing
	if open, known := none.Lists("octo/tools", 544); open || known {
		t.Fatalf("a nil listing knows nothing, got %v, %v", open, known)
	}
}

// TestFetchClaimScan_RecordsOpenListing: the scan records every open PR it
// paged through, including ones that reference no issue, and marks the repo
// complete once the listing reached its last page. No extra request is made.
func TestFetchClaimScan_RecordsOpenListing(t *testing.T) {
	open := []map[string]any{
		{
			"number": 544, "title": "rework the shared CI", "body": "No issue reference here.", "state": "open",
			"user": map[string]any{"login": "someone-else"}, "head": map[string]any{"ref": "ci-rework", "sha": "aaa"},
			"html_url": "https://github.com/torch-spyre/spyre-inference/pull/544",
		},
	}
	srv := mergedClaimServer(t, open, nil)
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	scan, err := c.FetchClaimScan(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"})
	if err != nil {
		t.Fatalf("FetchClaimScan: %v", err)
	}
	if len(scan.Claims) != 0 {
		t.Fatalf("a PR with no reference claims nothing, got %+v", scan.Claims)
	}
	if o, known := scan.OpenPRs.Lists("torch-spyre/spyre-inference", 544); !o || !known {
		t.Fatalf("Lists(544) = %v, %v; want the listed PR recorded open", o, known)
	}
	if o, known := scan.OpenPRs.Lists("torch-spyre/spyre-inference", 999); o || !known {
		t.Fatalf("Lists(999) = %v, %v; want a complete listing to know an absent PR is not open", o, known)
	}
}

// TestOpenPRListing_TwoOrgsSameName: a hive watching org1/actions and
// org2/actions. One org's complete listing must never decide a claim on the
// other org's PR, in either direction.
func TestOpenPRListing_TwoOrgsSameName(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	claimOn := func(repo string) IssueClaim {
		c := openPRVerdictClaim(now.Add(-time.Hour))
		c.Repo, c.PRRepo = repo, repo
		return c
	}
	// org1 listed completely, org2 failed to list (unknown).
	listing := completeListing("org1/actions", 700)

	t.Run("org1 listing does not drop an org2 claim whose PR is still open", func(t *testing.T) {
		if open, known := listing.Lists("org2/actions", 544); open || known {
			t.Fatalf("Lists(org2/actions, 544) = %v, %v; want unknown", open, known)
		}
		now := now
		l := settledLedger(t, &now)
		if err := l.Record(claimOn("org2/actions")); err != nil {
			t.Fatal(err)
		}
		l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: listing}, true, nil)
		got, ok := l.Lookup("org2/actions", 543)
		if !ok || !got.ObservedAt.Equal(now.Add(-time.Hour)) {
			t.Fatalf("the org2 claim must stay on its TTL, unconfirmed: %+v (found %v)", got, ok)
		}
	})

	t.Run("org1 listing does not confirm an org2 claim on a same-numbered PR", func(t *testing.T) {
		withSameNumber := completeListing("org1/actions", 544)
		if open, known := withSameNumber.Lists("org2/actions", 544); open || known {
			t.Fatalf("Lists(org2/actions, 544) = %v, %v; org1's #544 must not answer for org2", open, known)
		}
		now := now
		l := settledLedger(t, &now)
		if err := l.Record(claimOn("org2/actions")); err != nil {
			t.Fatal(err)
		}
		now = now.Add(claimLedgerTTL)
		l.ReconcileScan(ClaimScan{StartedAt: now.Add(-time.Minute), OpenPRs: withSameNumber}, true, nil)
		if _, ok := l.Lookup("org2/actions", 543); ok {
			t.Fatal("org1's open #544 must not keep org2's claim alive past its TTL")
		}
	})

	t.Run("each org's own listing still decides its own claim", func(t *testing.T) {
		both := completeListing("org1/actions", 544)
		both.markComplete("org2", "actions")
		if open, known := both.Lists("org1/actions", 544); !open || !known {
			t.Fatalf("org1 #544 = %v, %v; want open", open, known)
		}
		if open, known := both.Lists("org2/actions", 544); open || !known {
			t.Fatalf("org2 #544 = %v, %v; want known closed", open, known)
		}
	})
}
