package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// Tests for hivecommons/hive#13241: reverts and issue re-opens recorded as
// change failures on the merged PR's evidence bundle without any new per-PR
// GitHub calls.

func newChangeFailureClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call %s %s: change-failure recording must not fetch", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })
	return c
}

func foldOne(b *evidence.Bundle) *evidence.PR {
	for _, p := range evidence.FoldPRs([]*evidence.Bundle{b}, "", "") {
		return p
	}
	return nil
}

func loadEvidence(t *testing.T, root string, number int, head string) *evidence.Bundle {
	t.Helper()
	b, err := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", number, head))
	if err != nil {
		t.Fatalf("load %d@%s: %v", number, head, err)
	}
	return b
}

func TestRecordReviewEvidenceRevertLinksByNumberBodyAndSHA(t *testing.T) {
	root := withEvidenceRoot(t)
	now := time.Now().UTC()
	writeMergedEvidence(t, root, 5, "h5", now.Add(-24*time.Hour))          // named by title
	bySHA := writeMergedEvidence(t, root, 6, "h6", now.Add(-48*time.Hour)) // named by merge SHA in body
	bySHA.Merge.SHA = "abcdef1234567890abcdef1234567890abcdef12"
	if err := evidence.Seal(bySHA, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(ReviewEvidencePath(root, "o/r", 6, "h6"), bySHA); err != nil {
		t.Fatal(err)
	}
	stale := writeMergedEvidence(t, root, 4, "h4", now.Add(-20*24*time.Hour)) // outside 14 d
	open := writeMergedEvidence(t, root, 3, "h3", time.Time{})                // never merged
	c := newChangeFailureClient(t)

	c.RecordReviewEvidenceRevert(MergedPRText{
		Repo: "r", Number: 9, HeadSHA: "h9", MergedAt: now,
		Title: `Revert "thing" (#5)`,
		Body:  "This reverts commit abcdef12345.\n\nAlso touches #4 and #3.",
	})

	from := evidence.RevertBundleID("o/r", 9, "h9")
	for _, tc := range []struct {
		n    int
		head string
	}{{5, "h5"}, {6, "h6"}} {
		b := loadEvidence(t, root, tc.n, tc.head)
		if len(b.RevertedBy) != 1 || b.RevertedBy[0] != from {
			t.Errorf("PR %d reverted_by = %v, want [%s]", tc.n, b.RevertedBy, from)
		}
		if h, _ := evidence.Hash(b); h != b.Hash {
			t.Errorf("PR %d hash not re-sealed", tc.n)
		}
		if rv, _ := foldOne(b).ChangeFailure(); !rv {
			t.Errorf("PR %d must count as reverted", tc.n)
		}
	}
	if b := loadEvidence(t, root, 4, "h4"); len(b.RevertedBy) != 0 || b.Hash != stale.Hash {
		t.Errorf("PR merged 20 d ago must not be linked: %v", b.RevertedBy)
	}
	if b := loadEvidence(t, root, 3, "h3"); len(b.RevertedBy) != 0 || b.Hash != open.Hash {
		t.Errorf("unmerged PR must not be linked: %v", b.RevertedBy)
	}

	// Recording the same revert twice is idempotent.
	c.RecordReviewEvidenceRevert(MergedPRText{Repo: "o/r", Number: 9, HeadSHA: "h9", MergedAt: now, Title: "Reverts #5"})
	if b := loadEvidence(t, root, 5, "h5"); len(b.RevertedBy) != 1 {
		t.Errorf("duplicate revert recorded: %v", b.RevertedBy)
	}
}

func TestRecordReviewEvidenceRevertIgnoresNonRevertsAndDisabledEvidence(t *testing.T) {
	root := withEvidenceRoot(t)
	now := time.Now().UTC()
	orig := writeMergedEvidence(t, root, 5, "h5", now.Add(-time.Hour))
	c := newChangeFailureClient(t)

	c.RecordReviewEvidenceRevert(MergedPRText{Repo: "o/r", Number: 9, MergedAt: now, Title: "✨ follow-up to #5", Body: "Fixes #5"})
	c.RecordReviewEvidenceRevert(MergedPRText{Repo: "o/r", Number: 9, Title: "Reverts #5"}) // not merged
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{} })
	c.RecordReviewEvidenceRevert(MergedPRText{Repo: "o/r", Number: 9, MergedAt: now, Title: "Reverts #5"})

	if b := loadEvidence(t, root, 5, "h5"); len(b.RevertedBy) != 0 || b.Hash != orig.Hash {
		t.Errorf("bundle changed: reverted_by=%v", b.RevertedBy)
	}
}

func TestRecordReviewEvidenceIssueReopenStampsMergedBundle(t *testing.T) {
	root := withEvidenceRoot(t)
	mergedAt := time.Date(2026, 10, 14, 1, 46, 50, 0, time.UTC)
	writeMergedEvidence(t, root, 42, "h42a", time.Time{}) // superseded unmerged record
	writeMergedEvidence(t, root, 42, "h42", mergedAt)
	c := newChangeFailureClient(t)
	claim := IssueClaim{Repo: "r", Issue: 7, PRNumber: 42, PRRepo: "r", MergedPR: true, MergedAt: mergedAt}

	c.RecordReviewEvidenceIssueReopen(claim, mergedAt.Add(90*time.Minute))
	// Same issue again: no duplicate. Another issue 30 days out: outside the window.
	c.RecordReviewEvidenceIssueReopen(claim, mergedAt.Add(2*time.Hour))
	late := claim
	late.Issue = 8
	c.RecordReviewEvidenceIssueReopen(late, mergedAt.Add(30*24*time.Hour))

	b := loadEvidence(t, root, 42, "h42")
	if len(b.IssueReopens) != 1 {
		t.Fatalf("issue_reopens = %+v, want exactly one", b.IssueReopens)
	}
	r := b.IssueReopens[0]
	if r.Issue != 7 || r.Repo != "" || r.Source != evidence.IssueReopenSourceClaimLedger || !r.At.Equal(mergedAt.Add(90*time.Minute)) {
		t.Errorf("reopen = %+v", r)
	}
	if h, _ := evidence.Hash(b); h != b.Hash {
		t.Error("bundle not re-sealed")
	}
	if err := b.Validate(); err != nil {
		t.Errorf("stamped bundle invalid: %v", err)
	}
	if _, ro := foldOne(b).ChangeFailure(); !ro {
		t.Error("PR must count as re-opened")
	}
	if u := loadEvidence(t, root, 42, "h42a"); len(u.IssueReopens) != 0 {
		t.Errorf("unmerged record must stay untouched: %+v", u.IssueReopens)
	}
	// Reloading from disk is the restart path: the ledger survives.
	entries, err := ListReviewEvidence("", "o/r", 42)
	if err != nil || len(entries) != 2 {
		t.Fatalf("list = %d entries, err %v", len(entries), err)
	}
}

func TestRecordReviewEvidenceIssueReopenCrossRepoAndNoBundle(t *testing.T) {
	root := withEvidenceRoot(t)
	mergedAt := time.Date(2026, 10, 14, 1, 46, 50, 0, time.UTC)
	writeMergedEvidence(t, root, 42, "h42", mergedAt)
	c := newChangeFailureClient(t)

	c.RecordReviewEvidenceIssueReopen(IssueClaim{Repo: "o/docs", Issue: 3, PRNumber: 42, PRRepo: "o/r", MergedAt: mergedAt}, mergedAt.Add(time.Hour))
	c.RecordReviewEvidenceIssueReopen(IssueClaim{Repo: "r", Issue: 4, PRNumber: 99, PRRepo: "r", MergedAt: mergedAt}, mergedAt.Add(time.Hour))
	c.RecordReviewEvidenceIssueReopen(IssueClaim{}, mergedAt)

	b := loadEvidence(t, root, 42, "h42")
	if len(b.IssueReopens) != 1 || b.IssueReopens[0].Repo != "o/docs" || b.IssueReopens[0].Issue != 3 {
		t.Errorf("cross-repo reopen = %+v", b.IssueReopens)
	}
}

func TestMarkIssueReopenedNotifiesRecorder(t *testing.T) {
	var got []IssueClaim
	var gotAt time.Time
	SetIssueReopenRecorder(func(c IssueClaim, at time.Time) { got = append(got, c); gotAt = at })
	t.Cleanup(func() { SetIssueReopenRecorder(nil) })

	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)
	if ledger.MarkIssueReopened("r", 7, reopenTestMerged.Add(-time.Hour)) || len(got) != 0 {
		t.Fatal("a reopen before the merge must not reach the recorder")
	}
	if !ledger.MarkIssueReopened("r", 7, reopenTestReopened) {
		t.Fatal("reopen after merge must void the claim")
	}
	if len(got) != 1 || got[0].PRNumber != 42 || got[0].Issue != 7 || !gotAt.Equal(reopenTestReopened) {
		t.Fatalf("recorder got %+v at %v", got, gotAt)
	}
}

func TestWebhookMergedPRExtractsRevertText(t *testing.T) {
	mergedAt := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	raw := `{"action":"closed","number":9,"pull_request":{"number":9,"merged":true,"merged_at":"2026-10-20T10:00:00Z","title":"Revert \"x\"","body":"This reverts commit abc1234.","head":{"sha":"h9"}},"repository":{"full_name":"o/r"}}`
	var p webhookEventPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	got, ok := webhookMergedPR("pull_request", p)
	if !ok {
		t.Fatal("merged closed PR must be extracted")
	}
	want := MergedPRText{Repo: "o/r", Number: 9, HeadSHA: "h9", Title: `Revert "x"`, Body: "This reverts commit abc1234.", MergedAt: mergedAt}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	p.PullRequest.Merged = false
	if _, ok := webhookMergedPR("pull_request", p); ok {
		t.Error("closed-without-merge must not be extracted")
	}
	p.PullRequest.Merged = true
	if _, ok := webhookMergedPR("issues", p); ok {
		t.Error("other events must not be extracted")
	}
	p.Action = "synchronize"
	if _, ok := webhookMergedPR("pull_request", p); ok {
		t.Error("non-closed actions must not be extracted")
	}
}

func TestHandleWebhookEventMergedRevertLinksOriginal(t *testing.T) {
	root := withEvidenceRoot(t)
	c, clock := setupWebhookCacheTest(t)
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })
	mergedAt := clock.now.Add(-time.Hour)
	// setupWebhookCacheTest configures org/repo; writeMergedEvidence writes o/r,
	// so build the original bundle for org/repo by hand.
	orig := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID("org/repo", 5, "h5"),
		Repo: "org/repo", Number: 5, Author: evidence.Author{Login: "bot", Kind: evidence.AuthorAgent},
		BaseSHA: "base000", HeadSHA: "h5", CreatedAt: mergedAt.Add(-time.Hour), UpdatedAt: mergedAt,
		Merge: &evidence.MergeEvent{Actor: "a", Method: "squash", At: mergedAt, SHA: "m5"},
	}
	if err := evidence.Seal(orig, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(ReviewEvidencePath(root, "org/repo", 5, "h5"), orig); err != nil {
		t.Fatal(err)
	}

	payload := `{"action":"closed","number":9,"pull_request":{"number":9,"merged":true,"title":"Reverts #5","body":"","head":{"sha":"h9"}},"repository":{"full_name":"org/repo"}}`
	if _, err := c.HandleWebhookEvent("pull_request", []byte(payload)); err != nil {
		t.Fatal(err)
	}
	b, err := LoadReviewEvidence(ReviewEvidencePath(root, "org/repo", 5, "h5"))
	if err != nil {
		t.Fatal(err)
	}
	if want := evidence.RevertBundleID("org/repo", 9, "h9"); len(b.RevertedBy) != 1 || b.RevertedBy[0] != want {
		t.Errorf("reverted_by = %v, want [%s]", b.RevertedBy, want)
	}
}
