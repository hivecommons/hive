package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
)

// Coverage for hivecommons/hive#17435: the one digest comment on the pinned
// advisory issue was rewritten every eval cycle (~every 5 minutes on
// hivecommons/hive#2364) because live queue counts in the body drift each
// cycle. A changed digest is now written at most once per
// governor.advisory.digest_min_interval, in place, with a marker and an
// "Updated N times" footer whose counter survives restarts.

// countingDigestAt renders a 1-finding digest whose title carries a live
// count, the shape of drift that defeated the material fingerprint on #2364.
func countingDigestAt(at time.Time, queued int) string {
	return advisory.FormatDigestMarkdown(&advisory.Digest{
		GeneratedAt: at,
		Mode:        "advisory",
		ByAgent: map[string][]advisory.Finding{"scanner": {{
			Agent: "scanner", Timestamp: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			Type: "queue", Severity: "high",
			Title: fmt.Sprintf("governor actionable queue %d PRs", queued),
		}}},
		TotalCount: 1,
	}, advisory.DigestOptions{Org: "testorg", PrimaryRepo: "testrepo"})
}

func TestEvaluateDigestSuppression_MinIntervalHoldsChangedContent(t *testing.T) {
	t1 := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	posted := countingDigestAt(t1, 10)
	pending := countingDigestAt(t1.Add(5*time.Minute), 11)
	if materialDigestFingerprint(posted) == materialDigestFingerprint(pending) {
		t.Fatal("fixtures fingerprint identically; the min-interval branch would never be reached")
	}

	sup := evaluateDigestSuppression(pending, posted, true, t1, t1.Add(5*time.Minute), time.Hour)
	if !sup.suppress || sup.reason != suppressReasonMinInterval {
		t.Fatalf("changed content 5m after the last write: suppress=%v reason=%q, want %q", sup.suppress, sup.reason, suppressReasonMinInterval)
	}
	if sup := evaluateDigestSuppression(pending, posted, true, t1, t1.Add(61*time.Minute), time.Hour); sup.suppress {
		t.Fatalf("changed content after the window suppressed (%q)", sup.reason)
	}
	if sup := evaluateDigestSuppression(pending, posted, true, t1, t1.Add(5*time.Minute), 0); sup.suppress {
		t.Fatalf("interval 0 must disable the floor, got %q", sup.reason)
	}
}

func TestMaterialDigestContent_IgnoresMarkerAndUpdateFooter(t *testing.T) {
	body := countingDigestAt(time.Now(), 10)
	a := wrapDigestComment(body, 3, time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC))
	b := wrapDigestComment(body, 44, time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC))
	if a == b {
		t.Fatal("wrapper variants are identical; nothing is being tested")
	}
	if materialDigestFingerprint(a) != materialDigestFingerprint(body) {
		t.Errorf("marker/footer leaked into the fingerprint:\n%s", materialDigestContent(a))
	}
	if materialDigestFingerprint(a) != materialDigestFingerprint(b) {
		t.Error("two wraps of the same digest fingerprint differently")
	}
	if got := parseDigestUpdates(b); got != 44 {
		t.Errorf("parseDigestUpdates = %d, want 44", got)
	}
	if got := parseDigestUpdates(body); got != 0 {
		t.Errorf("legacy body parseDigestUpdates = %d, want 0", got)
	}
}

// Unchanged digest → no write at all, with the floor configured.
func TestPostAdvisoryDigest_MinIntervalUnchangedWritesNothing(t *testing.T) {
	org, repo := "testorg", "testrepo"
	start := time.Now().Add(-3 * time.Hour)
	srv := newRepeatDigestServer(t, org, repo, wrapDigestComment(countingDigestAt(start, 10), 7, start), start)
	c := newTestClient(t, srv.Server, org, []string{repo})
	c.SetAdvisoryDigestMinInterval(time.Hour)

	for i := 0; i < 4; i++ {
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, countingDigestAt(time.Now(), 10)); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if srv.writes() != 0 {
		t.Fatalf("unchanged digest wrote %d times, want 0", srv.writes())
	}
}

// Changed digest inside the window is held; after the window it is written
// as an EDIT of the marker comment, never a new comment, and the update
// counter continues from the one on the comment.
func TestPostAdvisoryDigest_MinIntervalHoldsThenEditsInPlace(t *testing.T) {
	org, repo := "testorg", "testrepo"
	lastWrite := time.Now().Add(-5 * time.Minute)
	srv := newRepeatDigestServer(t, org, repo, wrapDigestComment(countingDigestAt(lastWrite, 10), 7, lastWrite), lastWrite)
	c := newTestClient(t, srv.Server, org, []string{repo})
	c.SetAdvisoryDigestMinInterval(time.Hour)

	// Counts drift every cycle, as on hivecommons/hive#2364.
	for i := 0; i < 6; i++ {
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, countingDigestAt(time.Now(), 11+i)); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if srv.writes() != 0 {
		t.Fatalf("changed digest 5m after the last write wrote %d times, want 0 until the window passes", srv.writes())
	}

	// The window passes: GitHub's updated_at is now old enough.
	srv.updatedAt = time.Now().Add(-2 * time.Hour)
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, countingDigestAt(time.Now(), 20)); err != nil {
		t.Fatalf("post after window: %v", err)
	}
	if srv.editCalls != 1 || srv.createDone != 0 {
		t.Fatalf("after the window: edits=%d creates=%d, want exactly one in-place edit", srv.editCalls, srv.createDone)
	}
	if !strings.HasPrefix(srv.body, advisoryDigestMarker+" updates=8 -->") {
		t.Errorf("edited body does not carry the continued marker counter:\n%s", srv.body[:min(len(srv.body), 120)])
	}
	if !strings.Contains(srv.body, "_Updated 8 times · last ") {
		t.Errorf("edited body lacks the update footer:\n%s", srv.body)
	}

	// The write just happened: a further change is held again, this time by
	// the process-local last-write even though the server stamp is stale.
	srv.updatedAt = time.Now().Add(-2 * time.Hour)
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, countingDigestAt(time.Now(), 21)); err != nil {
		t.Fatalf("post right after write: %v", err)
	}
	if srv.editCalls != 1 {
		t.Fatalf("a change right after a write was written (edits=%d); the process-local last-write floor is not holding", srv.editCalls)
	}
}

// A fresh process (empty in-memory state) must not repost inside the window:
// the baseline is the comment's own updated_at on GitHub.
func TestPostAdvisoryDigest_MinIntervalSurvivesRestart(t *testing.T) {
	org, repo := "testorg", "testrepo"
	lastWrite := time.Now().Add(-10 * time.Minute)
	srv := newRepeatDigestServer(t, org, repo, wrapDigestComment(countingDigestAt(lastWrite, 10), 3, lastWrite), lastWrite)

	for i := 0; i < 4; i++ {
		c := newTestClient(t, srv.Server, org, []string{repo})
		c.SetAdvisoryDigestMinInterval(time.Hour)
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, countingDigestAt(time.Now(), 12+i)); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
	}
	if srv.writes() != 0 {
		t.Fatalf("after 4 restarts inside the window: writes=%d, want 0", srv.writes())
	}
}

// A legacy comment (bare heading, no marker) is adopted and gains the marker
// on its first edit; the first write on a bare target is a single create.
func TestPostAdvisoryDigest_LegacyCommentGainsMarker(t *testing.T) {
	org, repo := "testorg", "testrepo"
	old := time.Now().Add(-3 * time.Hour)
	srv := newRepeatDigestServer(t, org, repo, countingDigestAt(old, 10), old)
	c := newTestClient(t, srv.Server, org, []string{repo})
	c.SetAdvisoryDigestMinInterval(time.Hour)

	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, countingDigestAt(time.Now(), 11)); err != nil {
		t.Fatalf("post: %v", err)
	}
	if srv.editCalls != 1 || srv.createDone != 0 {
		t.Fatalf("legacy comment: edits=%d creates=%d, want one edit", srv.editCalls, srv.createDone)
	}
	if !strings.HasPrefix(srv.body, advisoryDigestMarker+" updates=1 -->") {
		t.Errorf("legacy comment did not gain the marker:\n%s", srv.body[:min(len(srv.body), 120)])
	}
	if !isAdvisoryDigestBody(srv.body) || !isAdvisoryDigestBody(countingDigestAt(old, 1)) {
		t.Error("isAdvisoryDigestBody must accept both the marker and the legacy heading")
	}
}
