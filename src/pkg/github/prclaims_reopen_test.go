package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// Regression for hivecommons/hive#9945: a merged PR closed the issue, the
// reporter reopened it, and the ledger kept treating the merged PR as the fix
// (likely-done / verify-once) so the issue sat in the queue with no work.

var (
	reopenTestMerged   = time.Date(2026, 10, 14, 1, 46, 50, 0, time.UTC)
	reopenTestReopened = reopenTestMerged.Add(90 * time.Minute)
)

func reopenTestClaim() IssueClaim {
	return IssueClaim{
		Repo: "r", Issue: 7, PRNumber: 42, PRRepo: "r", PRURL: "https://github.com/o/r/pull/42",
		MergedPR: true, MergedAt: reopenTestMerged, ObservedAt: reopenTestMerged, FirstObservedAt: reopenTestMerged,
	}
}

func TestMarkIssueReopenedVoidsMergedClaim(t *testing.T) {
	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)

	if ledger.MarkIssueReopened("r", 7, reopenTestMerged.Add(-time.Hour)) {
		t.Fatal("a reopen before the merge must not void the claim: the merge is the fix for it")
	}
	if ledger.MarkIssueReopened("r", 8, reopenTestReopened) {
		t.Fatal("unknown issue must report no change")
	}
	if _, ok := ledger.Lookup("r", 7); !ok {
		t.Fatal("claim must still be visible before the reopen")
	}

	if !ledger.MarkIssueReopened("r", 7, reopenTestReopened) {
		t.Fatal("reopen after merge must void the claim")
	}
	if ledger.MarkIssueReopened("r", 7, reopenTestReopened.Add(time.Hour)) {
		t.Fatal("second reopen must be a no-op")
	}
	if _, ok := ledger.Lookup("r", 7); ok {
		t.Fatal("Lookup must hide a voided claim so the guard treats the issue as unclaimed")
	}
	raw, ok := ledger.LookupAny("r", 7)
	if !ok || !raw.ReopenedAfterMerge() || !raw.ReopenedAt.Equal(reopenTestReopened) {
		t.Fatalf("LookupAny = %+v, %v; want voided record with ReopenedAt", raw, ok)
	}

	// A fresh scan of the same merged PR must not resurrect the claim.
	ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)
	if _, ok := ledger.Lookup("r", 7); ok {
		t.Fatal("Reconcile with the same PR must carry the reopen forward")
	}

	// Open-claim (not merged) records are never voided by a reopen.
	open := NewClaimLedger("", testLogger())
	c := reopenTestClaim()
	c.MergedPR, c.MergedAt = false, time.Time{}
	open.Reconcile([]IssueClaim{c}, true)
	if open.MarkIssueReopened("r", 7, reopenTestReopened) {
		t.Fatal("an open PR claim is not voided by a reopen")
	}
}

func TestFilterClaimedIssuesReopenedAfterMergeIsActionable(t *testing.T) {
	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)
	if !ledger.MarkIssueReopened("r", 7, reopenTestReopened) {
		t.Fatal("setup: reopen not recorded")
	}
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "r", Number: 7, Title: "Mysterious failure", Labels: []string{"bug", LikelyDoneLabel, CoveredByPRLabel}}}
	result.Issues.Count = 1

	if got := FilterClaimedIssues(result, ledger, func(string, int) bool { return true }, testLogger()); got != 0 {
		t.Fatalf("suppressed = %d, want 0", got)
	}
	if len(result.Issues.Items) != 1 {
		t.Fatalf("issue dropped: %+v", result.Issues.Items)
	}
	issue := result.Issues.Items[0]
	if issueHasLabel(issue.Labels, LikelyDoneLabel) || issueHasLabel(issue.Labels, CoveredByPRLabel) {
		t.Errorf("likely-done/covered-by labels must be cleared, got %v", issue.Labels)
	}
	cc := issue.ClaimContext
	if cc == nil || cc.Decision != ClaimDecisionReopenedAfterMerge || cc.MergedPR || cc.PRNumber != 42 || !cc.ReopenedAt.Equal(reopenTestReopened) {
		t.Fatalf("ClaimContext = %+v, want reopened_after_merge for PR 42 with MergedPR=false", cc)
	}
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Number != 42 || !issue.LinkedPRs[0].Merged {
		t.Errorf("LinkedPRs = %+v, want merged PR 42", issue.LinkedPRs)
	}
}

type reopenPollFixture struct {
	timeline       []map[string]any
	timelineStatus int
	timelineGets   int
}

func (f *reopenPollFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline" {
			f.timelineGets++
			if f.timelineStatus != 0 {
				w.WriteHeader(f.timelineStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(f.timeline)
			return
		}
		t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

func reopenPollResult(reopened bool, updatedAt time.Time) *ActionableResult {
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "r", Number: 7, Title: "t", Reopened: reopened, UpdatedAt: updatedAt, Labels: []string{LikelyDoneLabel}}}
	result.Issues.Count = 1
	return result
}

func TestReconcileReopenedIssuesVoidsClaimFromTimeline(t *testing.T) {
	identity := HiveIdentity{AppLogin: "hive[bot]"}
	now := reopenTestReopened.Add(time.Hour)

	t.Run("person reopened after merge", func(t *testing.T) {
		f := &reopenPollFixture{timeline: []map[string]any{
			reopenTimelineEvent("closed", reopenTestMerged.Format(time.RFC3339), "hive[bot]", "Bot"),
			reopenTimelineEvent("reopened", reopenTestReopened.Format(time.RFC3339), "kubestellar-prow[bot]", "Bot"),
		}}
		srv := f.server(t)
		defer srv.Close()
		c := newTestClient(t, srv, "o", []string{"r"})
		ledger := NewClaimLedger("", testLogger())
		ledger.SetClock(func() time.Time { return now })
		ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)

		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(true, now), testLogger()); got != 1 {
			t.Fatalf("voided = %d, want 1", got)
		}
		if _, ok := ledger.Lookup("r", 7); ok {
			t.Fatal("claim must be voided")
		}
		raw, _ := ledger.LookupAny("r", 7)
		if !raw.ReopenedAt.Equal(reopenTestReopened) {
			t.Fatalf("ReopenedAt = %v, want %v (the timeline instant, not now)", raw.ReopenedAt, reopenTestReopened)
		}
		// Already voided: no second timeline read.
		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(true, now), testLogger()); got != 0 || f.timelineGets != 1 {
			t.Fatalf("second pass voided=%d gets=%d, want 0 and 1", got, f.timelineGets)
		}
	})

	t.Run("not reopened issues and closed-only timelines cost at most one read per activity", func(t *testing.T) {
		f := &reopenPollFixture{timeline: []map[string]any{
			reopenTimelineEvent("closed", reopenTestMerged.Format(time.RFC3339), "hive[bot]", "Bot"),
		}}
		srv := f.server(t)
		defer srv.Close()
		c := newTestClient(t, srv, "o", []string{"r"})
		ledger := NewClaimLedger("", testLogger())
		ledger.SetClock(func() time.Time { return now })
		ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)

		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(false, now), testLogger()); got != 0 || f.timelineGets != 0 {
			t.Fatalf("state_reason!=reopened: voided=%d gets=%d, want 0/0", got, f.timelineGets)
		}
		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(true, now.Add(-time.Minute)), testLogger()); got != 0 || f.timelineGets != 1 {
			t.Fatalf("reopened without post-merge reopen event: voided=%d gets=%d, want 0/1", got, f.timelineGets)
		}
		if _, ok := ledger.Lookup("r", 7); !ok {
			t.Fatal("claim must survive when the timeline shows no post-merge reopen")
		}
		// No new activity since the check: no further read.
		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(true, now.Add(-time.Minute)), testLogger()); got != 0 || f.timelineGets != 1 {
			t.Fatalf("unchanged issue re-read: gets=%d, want 1", f.timelineGets)
		}
		// New activity: read again.
		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(true, now.Add(time.Minute)), testLogger()); got != 0 || f.timelineGets != 2 {
			t.Fatalf("updated issue not re-read: gets=%d, want 2", f.timelineGets)
		}
	})

	t.Run("hive's own reopen does not count", func(t *testing.T) {
		f := &reopenPollFixture{timeline: []map[string]any{
			reopenTimelineEvent("reopened", reopenTestReopened.Format(time.RFC3339), "hive[bot]", "Bot"),
		}}
		srv := f.server(t)
		defer srv.Close()
		c := newTestClient(t, srv, "o", []string{"r"})
		ledger := NewClaimLedger("", testLogger())
		ledger.SetClock(func() time.Time { return now })
		ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)
		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(true, now), testLogger()); got != 0 {
			t.Fatalf("voided = %d, want 0", got)
		}
	})

	t.Run("timeline error keeps the claim for the next pass", func(t *testing.T) {
		f := &reopenPollFixture{timelineStatus: http.StatusForbidden}
		srv := f.server(t)
		defer srv.Close()
		c := newTestClient(t, srv, "o", []string{"r"})
		ledger := NewClaimLedger("", testLogger())
		ledger.SetClock(func() time.Time { return now })
		ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)
		if got := ledger.ReconcileReopenedIssues(context.Background(), c, identity, reopenPollResult(true, now), testLogger()); got != 0 {
			t.Fatalf("voided = %d, want 0", got)
		}
		if _, ok := ledger.Lookup("r", 7); !ok {
			t.Fatal("claim must survive a failed timeline read")
		}
		raw, _ := ledger.LookupAny("r", 7)
		if !raw.ReopenCheckedAt.IsZero() {
			t.Fatal("a failed read must not be recorded as checked")
		}
	})

	if ledger := (*ClaimLedger)(nil); ledger.ReconcileReopenedIssues(context.Background(), nil, identity, nil, nil) != 0 {
		t.Fatal("nil ledger must be a no-op")
	}
}

func TestReopenedClaimPersistsAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	ledger := NewClaimLedger(path, testLogger())
	ledger.Reconcile([]IssueClaim{reopenTestClaim()}, true)
	if !ledger.MarkIssueReopened("r", 7, reopenTestReopened) {
		t.Fatal("setup: reopen not recorded")
	}
	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup("r", 7); ok {
		t.Fatal("voided claim must stay voided after reload")
	}
	raw, ok := reloaded.LookupAny("r", 7)
	if !ok || !raw.ReopenedAt.Equal(reopenTestReopened) {
		t.Fatalf("reloaded = %+v, %v", raw, ok)
	}
}

func TestCloseOnMergeLedgerSweepSkipsReopenedClaims(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := newLedgerSweepFixture(t, 7)
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	if err := ledger.Record(mergedLedgerClaim(7, reopenTestMerged)); err != nil {
		t.Fatal(err)
	}
	if !ledger.MarkIssueReopened("o/r", 7, reopenTestReopened) {
		t.Fatal("setup: reopen not recorded")
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}, Now: func() time.Time { return reopenTestReopened.Add(time.Hour) }}

	sweep := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if sweep.SkippedReopened != 1 || sweep.Marked != 0 || len(sweep.Results) != 1 || sweep.Results[0].Action != CloseOnMergeSkipped || sweep.Results[0].Reason != "reopened_after_merge" {
		t.Fatalf("sweep = %+v, want one reopened skip", sweep)
	}
	if f.count("issue_get") != 0 || f.count("pull_get") != 0 || f.count("patch") != 0 {
		t.Fatalf("reopened claim must cost no API call and never close the issue: %+v", f)
	}
}

func TestHandleWebhookEventIssuesReopenedCallsHook(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	type call struct {
		repo string
		n    int
		at   time.Time
	}
	var calls []call
	SetIssueReopenedHook(func(repo string, number int, at time.Time) {
		calls = append(calls, call{repo, number, at})
	})
	t.Cleanup(func() { SetIssueReopenedHook(nil) })

	res, err := c.HandleWebhookEvent("issues", []byte(`{"action":"reopened","issue":{"number":7},"repository":{"full_name":"org/repo"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.KnownRepo || res.ReopenedIssue != 7 {
		t.Fatalf("result = %+v, want known repo and ReopenedIssue 7", res)
	}
	if len(calls) != 1 || calls[0].repo != "repo" || calls[0].n != 7 || !calls[0].at.Equal(clock.now) {
		t.Fatalf("hook calls = %+v, want one call for repo#7 at %v", calls, clock.now)
	}

	for name, payload := range map[string]string{
		"closed":          `{"action":"closed","issue":{"number":7},"repository":{"full_name":"org/repo"}}`,
		"reopened PR":     `{"action":"reopened","issue":{"number":7,"pull_request":{"url":"x"}},"repository":{"full_name":"org/repo"}}`,
		"reopened no num": `{"action":"reopened","issue":{},"repository":{"full_name":"org/repo"}}`,
	} {
		res, err := c.HandleWebhookEvent("issues", []byte(payload))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.ReopenedIssue != 0 || len(calls) != 1 {
			t.Fatalf("%s: hook must not fire: res=%+v calls=%d", name, res, len(calls))
		}
	}

	// No hook installed: event still accepted.
	SetIssueReopenedHook(nil)
	if _, err := c.HandleWebhookEvent("issues", []byte(`{"action":"reopened","issue":{"number":8},"repository":{"full_name":"org/repo"}}`)); err != nil {
		t.Fatal(err)
	}
}
