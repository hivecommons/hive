package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// coveredIssue seeds the shared store the default sink reads with an issue
// at 040 covered by o/r#8 (the PR itself staged 050), as the sweep or a
// CoveredBy fact leaves it, and forgets both when the test ends.
func coveredIssue(t *testing.T, c *Client) (issue, pr stageengine.Ref) {
	t.Helper()
	store := stagelabels.Shared()
	issue, pr = c.IssueStageRef("o/r", 7), c.PRStageRef("o/r", 8)
	store.Observe(issue.Key(), stagelabels.CoveredByPR, "open PR covers it")
	store.SetCoveringPR(issue.Key(), pr.Key())
	store.Observe(pr.Key(), stagelabels.ReviewPending, "required-check-pending")
	t.Cleanup(func() { store.Forget(issue.Key()); store.Forget(pr.Key()) })
	return issue, pr
}

// A contributor claim on an issue an open PR already covers (hotshot#269:
// #270 open, then a relay claim) is not a step backwards: the fresh read
// finds the PR open, the issue stays 040 naming it, and the store books no
// regression. The release that follows leaves 040 standing the same way.
func TestStageFacts_ClaimAndReleaseUnderOpenCoveringPRKeep040(t *testing.T) {
	srv := newStageFactsServer(t, "help wanted", stagelabels.CoveredByPR.Label())
	c := testClient(t, srv.srv.URL)
	issue, _ := coveredIssue(t, c)
	ctx := context.Background()

	if err := c.StageFacts().Claimed(ctx, issue, stageengine.ClaimFact{Holder: "mendezr", Kind: claims.KindContributor}); err != nil {
		t.Fatal(err)
	}
	labels, added, removed := srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.CoveredByPR || len(added)+len(removed) != 0 {
		t.Fatalf("after claim: labels=%v added=%v removed=%v, want 040 untouched", labels, added, removed)
	}
	rec, _ := stagelabels.Shared().Get(issue.Key())
	if rec.Stage != stagelabels.CoveredByPR || rec.Regressed() || !strings.Contains(rec.Reason, "#8") {
		t.Fatalf("record after claim = %+v, want 040 naming #8 and no regression", rec)
	}
	if srv.prReads != 1 {
		t.Fatalf("covering PR read %d times, want once", srv.prReads)
	}

	if err := c.StageFacts().ClaimReleased(ctx, issue, "mendezr"); err != nil {
		t.Fatal(err)
	}
	labels, added, removed = srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.CoveredByPR || len(added)+len(removed) != 0 {
		t.Fatalf("after release: labels=%v added=%v removed=%v, want 040 untouched", labels, added, removed)
	}
	if rec, _ := stagelabels.Shared().Get(issue.Key()); rec.Stage != stagelabels.CoveredByPR || rec.Regressed() {
		t.Fatalf("record after release = %+v, want 040 and no regression", rec)
	}
}

// The same facts once the covering PR closed unmerged: the memory is stale,
// the fresh read says so, and the claim / release derive 020 / 010 as before.
func TestStageFacts_ClaimUnderClosedCoveringPRDerivesClaimed(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.CoveredByPR.Label())
	srv.prState = "closed"
	c := testClient(t, srv.srv.URL)
	issue, _ := coveredIssue(t, c)

	if err := c.StageFacts().Claimed(context.Background(), issue, stageengine.ClaimFact{Holder: "mendezr", Kind: claims.KindContributor}); err != nil {
		t.Fatal(err)
	}
	labels, _, _ := srv.snapshot()
	if stagelabels.Current(labels) != stagelabels.Claimed {
		t.Fatalf("labels = %v, want 020 once the cover is gone", labels)
	}
	if rec, _ := stagelabels.Shared().Get(issue.Key()); rec.CoveringPR != "" {
		t.Fatalf("stale covering PR kept after leaving 040: %q", rec.CoveringPR)
	}
}

// A merged covering PR is no cover either (the sweep closes or verifies
// the issue; the fact derives what it says).
func TestStageFacts_ReleaseUnderMergedCoveringPRDerivesActionable(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.CoveredByPR.Label())
	srv.prMerged = true
	c := testClient(t, srv.srv.URL)
	issue, _ := coveredIssue(t, c)

	if err := c.StageFacts().ClaimReleased(context.Background(), issue, "mendezr"); err != nil {
		t.Fatal(err)
	}
	if labels, _, _ := srv.snapshot(); stagelabels.Current(labels) != stagelabels.Actionable {
		t.Fatalf("labels = %v, want 010", labels)
	}
}

// Uncovered names the PR that closed: the store's memory of that same PR
// is not consulted (no re-read), and the issue goes to 010.
func TestStageFacts_UncoveredByTheRememberedPRSkipsReread(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.CoveredByPR.Label())
	c := testClient(t, srv.srv.URL)
	issue, pr := coveredIssue(t, c)

	if err := c.StageFacts().Uncovered(context.Background(), issue, pr); err != nil {
		t.Fatal(err)
	}
	if labels, _, _ := srv.snapshot(); stagelabels.Current(labels) != stagelabels.Actionable {
		t.Fatalf("labels = %v, want 010", labels)
	}
	if srv.prReads != 0 {
		t.Fatalf("the closed PR was re-read %d times", srv.prReads)
	}
}

// An issue the store does not hold at 040 costs no PR read: the fact
// derives as submitted.
func TestStageFacts_UncoveredIssueNeedsNoPRRead(t *testing.T) {
	srv := newStageFactsServer(t, stagelabels.Actionable.Label())
	c := testClient(t, srv.srv.URL)
	issue := c.IssueStageRef("o/r", 7)
	t.Cleanup(func() { stagelabels.Shared().Forget(issue.Key()) })

	if err := c.StageFacts().Claimed(context.Background(), issue, stageengine.ClaimFact{Holder: "hive-a"}); err != nil {
		t.Fatal(err)
	}
	if labels, _, _ := srv.snapshot(); stagelabels.Current(labels) != stagelabels.Claimed {
		t.Fatalf("labels = %v, want 020", labels)
	}
	if srv.prReads != 0 {
		t.Fatalf("PR read %d times for an uncovered issue", srv.prReads)
	}
}

func TestParseStoreKeyRef(t *testing.T) {
	ref, ok := parseStoreKeyRef("hivecommons/hotshot#270")
	if !ok || ref != (stageengine.Ref{Owner: "hivecommons", Repo: "hotshot", Number: 270, Kind: stagelabels.PRItem}) {
		t.Fatalf("parse = %+v ok=%v", ref, ok)
	}
	for _, bad := range []string{"", "o/r", "o#7", "o/r#x", "o/r#0", "gitea:o/r#7", "/r#7"} {
		if _, ok := parseStoreKeyRef(bad); ok {
			t.Errorf("parseStoreKeyRef(%q) accepted", bad)
		}
	}
}
