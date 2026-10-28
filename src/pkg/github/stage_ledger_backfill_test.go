package github

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// claimedIssue seeds the shared store with o/r#7 at 020 under a live claim
// and no memory of any covering PR — the state a covered issue was in when
// its PR's webhook landed in the ledger but no stage fact followed.
func claimedIssue(t *testing.T, c *Client) stageengine.Ref {
	t.Helper()
	store := stagelabels.Shared()
	issue := c.IssueStageRef("o/r", 7)
	store.Observe(issue.Key(), stagelabels.Claimed, "live issue claim")
	t.Cleanup(func() { store.Forget(issue.Key()); store.Forget(c.PRStageRef("o/r", 8).Key()) })
	return issue
}

func ledgerLookupFor(claim IssueClaim, ok bool) func(repo string, number int) (IssueClaim, bool) {
	return func(repo string, number int) (IssueClaim, bool) {
		if number != claim.Issue || (repo != claim.Repo && repo != "o/"+claim.Repo) {
			return IssueClaim{}, false
		}
		return claim, ok
	}
}

// hivecommons/hive#14910 et al.: the store holds 020 and nothing about a
// covering PR, the claim ledger holds the open PR that fixes the issue
// (recorded from its webhook), and the claim is released. The release must
// re-derive 040 under that PR, not fall to 010.
func TestStageFacts_ClaimReleaseUnderLedgerCoveringPRDerives040(t *testing.T) {
	srv := newStageFactsServer(t, "help wanted", stagelabels.Claimed.Label())
	c := testClient(t, srv.srv.URL)
	c.coveredByPRLabel.mark("o/r")
	issue := claimedIssue(t, c)
	c.SetPRClaimLookup(ledgerLookupFor(IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", PRAuthor: "hive[bot]", Closing: true}, true))

	if err := c.StageFacts().ClaimReleased(context.Background(), issue, "mendezr"); err != nil {
		t.Fatal(err)
	}
	labels, _, _ := srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.CoveredByPR {
		t.Fatalf("labels after release = %v, want 040", labels)
	}
	rec, _ := stagelabels.Shared().Get(issue.Key())
	if rec.Stage != stagelabels.CoveredByPR || rec.CoveringPR != c.PRStageRef("o/r", 8).Key() {
		t.Fatalf("record after release = %+v, want 040 naming o/r#8", rec)
	}
	if srv.prReads != 1 {
		t.Fatalf("covering PR read %d times, want once (liveness check)", srv.prReads)
	}
}

// The same release under an EXTERNAL contributor's open PR: the guard
// buckets it claimed_by_pr too, so the fact lands on 040 as well.
func TestStageFacts_ClaimReleaseUnderExternalLedgerClaimDerives040(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.Claimed.Label())
	c := testClient(t, srv.srv.URL)
	c.coveredByPRLabel.mark("o/r")
	issue := claimedIssue(t, c)
	c.SetPRClaimLookup(ledgerLookupFor(IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 8, PRRepo: "o/r", PRAuthor: "mendezr", ExternalAuthor: true, Reference: true}, true))

	if err := c.StageFacts().ClaimReleased(context.Background(), issue, "mendezr"); err != nil {
		t.Fatal(err)
	}
	if labels, _, _ := srv.snapshot(); stagelabels.Current(labels) != stagelabels.CoveredByPR {
		t.Fatalf("labels = %v, want 040", labels)
	}
	// A "Refs #N" claim is 040 as "addresses part of it", never "covers it"
	// (hivecommons/hive#15008).
	rec, _ := stagelabels.Shared().Get(issue.Key())
	if !stageengine.ReasonIsPartialCover(rec.Reason) {
		t.Fatalf("record reason = %q, want the partial-cover reason for a non-closing reference claim", rec.Reason)
	}
}

// Ledger claims the guard does not suppress for are no cover: a hive
// non-closing reference annotates, a merged claim is a verification matter.
// Neither costs a PR read, and the release derives 010 as before.
func TestStageFacts_ClaimReleaseIgnoresWeakAndMergedLedgerClaims(t *testing.T) {
	for name, claim := range map[string]IssueClaim{
		"reference": {Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", Reference: true},
		"merged":    {Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", MergedPR: true, MergedAt: time.Now()},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newStageFactsServer(t, stagelabels.Claimed.Label())
			c := testClient(t, srv.srv.URL)
			issue := claimedIssue(t, c)
			c.SetPRClaimLookup(ledgerLookupFor(claim, true))

			if err := c.StageFacts().ClaimReleased(context.Background(), issue, "mendezr"); err != nil {
				t.Fatal(err)
			}
			if labels, _, _ := srv.snapshot(); stagelabels.Current(labels) != stagelabels.Actionable {
				t.Fatalf("labels = %v, want 010", labels)
			}
			if srv.prReads != 0 {
				t.Fatalf("PR read %d times for a non-covering claim", srv.prReads)
			}
		})
	}
}

// A ledger claim whose PR the fresh read finds closed is stale; the fact
// derives what it says and the store does not learn a cover.
func TestStageFacts_ClaimUnderClosedLedgerPRDerivesClaimed(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.Actionable.Label())
	srv.prState = "closed"
	c := testClient(t, srv.srv.URL)
	issue := c.IssueStageRef("o/r", 7)
	t.Cleanup(func() { stagelabels.Shared().Forget(issue.Key()) })
	c.SetPRClaimLookup(ledgerLookupFor(IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r"}, true))

	if err := c.StageFacts().Claimed(context.Background(), issue, stageengine.ClaimFact{Holder: "mendezr", Kind: claims.KindContributor}); err != nil {
		t.Fatal(err)
	}
	if labels, _, _ := srv.snapshot(); stagelabels.Current(labels) != stagelabels.Claimed {
		t.Fatalf("labels = %v, want 020", labels)
	}
	if rec, _ := stagelabels.Shared().Get(issue.Key()); rec.CoveringPR != "" {
		t.Fatalf("closed PR remembered as cover: %q", rec.CoveringPR)
	}
}

func TestIssueClaimCoversOpenIssue(t *testing.T) {
	cases := []struct {
		name  string
		claim IssueClaim
		want  bool
	}{
		{"hive closing", IssueClaim{PRNumber: 1, Closing: true}, true},
		{"hive plain", IssueClaim{PRNumber: 1}, true},
		{"hive reference", IssueClaim{PRNumber: 1, Reference: true}, false},
		{"external reference", IssueClaim{PRNumber: 1, Reference: true, ExternalAuthor: true}, true},
		{"external closing", IssueClaim{PRNumber: 1, ExternalAuthor: true}, true},
		{"merged", IssueClaim{PRNumber: 1, MergedPR: true}, false},
		{"no pr", IssueClaim{}, false},
	}
	for _, tc := range cases {
		if got := tc.claim.CoversOpenIssue(); got != tc.want {
			t.Errorf("%s: CoversOpenIssue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func backfillLedger(t *testing.T, claims ...IssueClaim) *ClaimLedger {
	t.Helper()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "claims.json"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, c := range claims {
		if c.ObservedAt.IsZero() {
			c.ObservedAt = time.Now()
		}
		if err := ledger.Record(c); err != nil {
			t.Fatal(err)
		}
	}
	return ledger
}

// The backfill: a record below 040 under a ledger claim an open PR makes is
// moved to 040 (one PR read, one label write); a record already at 040 and
// an issue the store never saw cost nothing.
func TestBackfillCoveredByLedger_HealsRecordBelow040(t *testing.T) {
	srv := newStageFactsServer(t, "help wanted", stagelabels.Actionable.Label())
	c := testClient(t, srv.srv.URL)
	c.coveredByPRLabel.mark("o/r")
	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: true} })
	store := stagelabels.Shared()
	issue := c.IssueStageRef("o/r", 7)
	store.Observe(issue.Key(), stagelabels.Actionable, "actionable")
	covered := c.IssueStageRef("o/r", 9)
	store.Observe(covered.Key(), stagelabels.CoveredByPR, "open PR covers it")
	t.Cleanup(func() {
		store.Forget(issue.Key())
		store.Forget(covered.Key())
		store.Forget(c.PRStageRef("o/r", 8).Key())
	})
	ledger := backfillLedger(t,
		IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", Closing: true},
		IssueClaim{Repo: "r", Issue: 9, PRNumber: 8, PRRepo: "r", Closing: true},
		IssueClaim{Repo: "r", Issue: 11, PRNumber: 8, PRRepo: "r", Closing: true},
	)

	res := c.BackfillCoveredByLedger(context.Background(), ledger, nil)
	if res.Checked != 1 || res.Covered != 1 || res.Errors != 0 {
		t.Fatalf("backfill = %+v, want checked=1 covered=1", res)
	}
	rec, _ := store.Get(issue.Key())
	if rec.Stage != stagelabels.CoveredByPR || rec.CoveringPR != c.PRStageRef("o/r", 8).Key() {
		t.Fatalf("record = %+v, want 040 under o/r#8", rec)
	}
	if labels, _, _ := srv.snapshot(); stagelabels.Current(labels) != stagelabels.CoveredByPR {
		t.Fatalf("labels = %v, want 040", labels)
	}
	if srv.prReads != 1 {
		t.Fatalf("PR read %d times, want once", srv.prReads)
	}

	// Converged: a second pass is a no-op without an API call.
	res = c.BackfillCoveredByLedger(context.Background(), ledger, nil)
	if res.Checked != 0 || srv.prReads != 1 {
		t.Fatalf("second pass = %+v prReads=%d, want nothing to do", res, srv.prReads)
	}
}

// The red+stale valve the guard applies holds here too, and a claim whose PR
// turns out merged is left to close-on-merge.
func TestBackfillCoveredByLedger_RespectsRedStaleAndMerged(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.Actionable.Label())
	c := testClient(t, srv.srv.URL)
	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: true} })
	store := stagelabels.Shared()
	issue := c.IssueStageRef("o/r", 7)
	store.Observe(issue.Key(), stagelabels.Actionable, "actionable")
	t.Cleanup(func() { store.Forget(issue.Key()) })
	ledger := backfillLedger(t, IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", Closing: true})

	res := c.BackfillCoveredByLedger(context.Background(), ledger, func(prRepo string, prNumber int) bool { return prNumber == 8 })
	if res.Checked != 1 || res.SkippedRedStale != 1 || res.Covered != 0 || srv.prReads != 0 {
		t.Fatalf("red+stale pass = %+v prReads=%d", res, srv.prReads)
	}

	srv.prMerged = true
	res = c.BackfillCoveredByLedger(context.Background(), ledger, nil)
	if res.Checked != 1 || res.SkippedNotOpen != 1 || res.Covered != 0 {
		t.Fatalf("merged pass = %+v", res)
	}
	if rec, _ := store.Get(issue.Key()); rec.Stage != stagelabels.Actionable {
		t.Fatalf("record = %+v, want untouched 010", rec)
	}
}

// With the issue-stage lane off nothing is written.
func TestBackfillCoveredByLedger_DisabledIsNoop(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.Actionable.Label())
	c := testClient(t, srv.srv.URL)
	store := stagelabels.Shared()
	issue := c.IssueStageRef("o/r", 7)
	store.Observe(issue.Key(), stagelabels.Actionable, "actionable")
	t.Cleanup(func() { store.Forget(issue.Key()) })
	ledger := backfillLedger(t, IssueClaim{Repo: "r", Issue: 7, PRNumber: 8, PRRepo: "r", Closing: true})

	if res := c.BackfillCoveredByLedger(context.Background(), ledger, nil); res != (LedgerCoverBackfill{}) || srv.prReads != 0 {
		t.Fatalf("disabled pass = %+v prReads=%d", res, srv.prReads)
	}
}

func TestCloseOnMergeOptionsWantsPR(t *testing.T) {
	any := CloseOnMergeOptions{}
	if !any.wantsPR("hive", 1) {
		t.Fatal("no filter must admit every PR")
	}
	only := CloseOnMergeOptions{OnlyPRRepo: "hivecommons/hive", OnlyPRNumber: 14920}
	if !only.wantsPR("hive", 14920) || !only.wantsPR("hivecommons/hive", 14920) {
		t.Fatal("bare and qualified spellings of the PR repo must match")
	}
	if only.wantsPR("hive", 14921) || only.wantsPR("crux", 14920) {
		t.Fatal("another PR admitted")
	}
}
