package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ledgerSweepFixture serves a repo whose issues are all closed except the
// ones in open, and counts every request by method+path prefix so a test can
// prove a sweep made no API call for a settled claim.
type ledgerSweepFixture struct {
	t    *testing.T
	mu   sync.Mutex
	open map[int]bool
	hits map[string]int
	// issueStatus, when non-zero, fails every issue GET with that status.
	issueStatus int
}

func newLedgerSweepFixture(t *testing.T, open ...int) *ledgerSweepFixture {
	f := &ledgerSweepFixture{t: t, open: map[int]bool{}, hits: map[string]int{}}
	for _, n := range open {
		f.open[n] = true
	}
	return f
}

func (f *ledgerSweepFixture) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[kind]
}

func (f *ledgerSweepFixture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		f.mu.Lock()
		switch {
		case r.Method == "GET" && strings.Contains(path, "/pulls/"):
			f.hits["pull_get"]++
		case r.Method == "GET" && strings.HasSuffix(path, "/comments"):
			f.hits["comments_get"]++
		case r.Method == "GET" && strings.HasSuffix(path, "/timeline"):
			f.hits["timeline_get"]++
		case r.Method == "GET" && strings.Contains(path, "/issues/"):
			f.hits["issue_get"]++
		case r.Method == "PATCH":
			f.hits["patch"]++
		case r.Method == "POST":
			f.hits["post"]++
		}
		f.mu.Unlock()
		// /repos/o/r/<issues|pulls>/<n>...; the fixture sets PR number ==
		// issue number so each PR body closes its own issue.
		n, _ := strconvAtoi(parts5(path))
		switch {
		case r.Method == "GET" && strings.Contains(path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": n, "title": "fix", "body": "Fixes #" + parts5(path), "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/fix", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "GET" && strings.HasSuffix(path, "/comments"):
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == "GET" && strings.HasSuffix(path, "/timeline"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "GET" && strings.Contains(path, "/issues/"):
			if f.issueStatus != 0 {
				w.WriteHeader(f.issueStatus)
				return
			}
			state := "closed"
			f.mu.Lock()
			if f.open[n] {
				state = "open"
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": state, "body": "body", "labels": []map[string]string{}})
		case r.Method == "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH":
			f.mu.Lock()
			delete(f.open, n)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "closed"})
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNotFound)
		default:
			f.t.Fatalf("unexpected request %s %s", r.Method, path)
		}
	}))
}

// parts5 returns the issue/PR number segment of /repos/o/r/<kind>/<n>...
func parts5(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 6 {
		return parts[5]
	}
	return "0"
}

func mergedLedgerClaim(issue int, mergedAt time.Time) IssueClaim {
	return IssueClaim{Repo: "o/r", Issue: issue, PRNumber: issue, PRRepo: "o/r", MergedPR: true, MergedAt: mergedAt, ObservedAt: mergedAt, FirstObservedAt: mergedAt}
}

func TestCloseOnMergeLedgerSweepSettledClaimsSkipAPI(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := newLedgerSweepFixture(t, 7)
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	merged := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, issue := range []int{7, 8, 9} {
		if err := ledger.Record(mergedLedgerClaim(issue, merged)); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	now := merged.Add(time.Hour)
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}, Now: func() time.Time { return now }}

	first := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(first.Results) != 3 || first.Marked != 3 || first.SkippedSettled != 0 {
		t.Fatalf("first sweep = %+v", first)
	}
	// Three claim reads plus CloseIssue's own re-read of issue 7.
	const firstSweepIssueGets = 4
	if got := f.count("issue_get"); got != firstSweepIssueGets {
		t.Fatalf("first sweep issue GETs = %d, want %d", got, firstSweepIssueGets)
	}
	if got := f.count("patch"); got != 1 {
		t.Fatalf("first sweep closes = %d, want 1 (issue 7)", got)
	}
	for _, claim := range ledger.Claims() {
		if !claim.CloseOnMergeSettledAt.Equal(now) {
			t.Fatalf("claim %d settled at %v, want %v", claim.Issue, claim.CloseOnMergeSettledAt, now)
		}
	}

	// A second sweep inside the recheck window makes no API call at all.
	now = now.Add(time.Hour)
	second := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(second.Results) != 0 || second.SkippedSettled != 3 || second.Marked != 0 {
		t.Fatalf("second sweep = %+v, want 3 settled skips", second)
	}
	if got := f.count("issue_get"); got != firstSweepIssueGets {
		t.Fatalf("second sweep added issue GETs: total %d, want %d", got, firstSweepIssueGets)
	}
	if got := f.count("pull_get"); got != 3 {
		t.Fatalf("second sweep added PR GETs: total %d, want 3", got)
	}

	// Past the recheck window each claim is verified once more and re-stamped.
	now = now.Add(DefaultCloseOnMergeRecheck)
	third := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(third.Results) != 3 || third.Marked != 3 {
		t.Fatalf("third sweep = %+v, want 3 rechecked", third)
	}
	// Issue 7 is closed now, so the recheck is one read per claim.
	if got := f.count("issue_get"); got != firstSweepIssueGets+3 {
		t.Fatalf("third sweep issue GETs total = %d, want %d", got, firstSweepIssueGets+3)
	}
	for _, claim := range ledger.Claims() {
		if !claim.CloseOnMergeSettledAt.Equal(now) {
			t.Fatalf("claim %d not re-stamped: %v", claim.Issue, claim.CloseOnMergeSettledAt)
		}
	}

	// The marker is persisted: a reloaded ledger skips too.
	reloaded, err := LoadClaimLedger(ledger.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.SetClock(func() time.Time { return now })
	fourth := c.CloseOnMergeLedgerSweep(context.Background(), reloaded, opts)
	if fourth.SkippedSettled != 3 || len(fourth.Results) != 0 {
		t.Fatalf("reloaded sweep = %+v, want 3 settled skips", fourth)
	}
}

func TestCloseOnMergeLedgerSweepCustomRecheckWindow(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := newLedgerSweepFixture(t)
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	merged := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := ledger.Record(mergedLedgerClaim(8, merged)); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	now := merged.Add(time.Minute)
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}, RecheckAfter: 30 * time.Minute, Now: func() time.Time { return now }}
	if s := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts); s.Marked != 1 {
		t.Fatalf("first = %+v", s)
	}
	now = now.Add(29 * time.Minute)
	if s := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts); s.SkippedSettled != 1 {
		t.Fatalf("inside window = %+v, want skip", s)
	}
	now = now.Add(2 * time.Minute)
	if s := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts); len(s.Results) != 1 || s.SkippedSettled != 0 {
		t.Fatalf("past window = %+v, want recheck", s)
	}
	if got := f.count("issue_get"); got != 2 {
		t.Fatalf("issue GETs = %d, want 2", got)
	}
}

func TestCloseOnMergeLedgerSweepTransientFailureDoesNotSettle(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := newLedgerSweepFixture(t, 7)
	f.issueStatus = http.StatusBadGateway
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	merged := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if err := ledger.Record(mergedLedgerClaim(7, merged)); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}, Now: func() time.Time { return merged.Add(time.Hour) }}
	s := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(s.Results) != 1 || s.Results[0].Reason != "issue_read_failed" || s.Marked != 0 {
		t.Fatalf("sweep = %+v, want unsettled issue_read_failed", s)
	}
	claim, _ := ledger.Lookup("o/r", 7)
	if !claim.CloseOnMergeSettledAt.IsZero() {
		t.Fatalf("transient failure stamped settled marker: %v", claim.CloseOnMergeSettledAt)
	}
	// The API recovers: the next sweep retries and closes.
	f.issueStatus = 0
	s = c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(s.Results) != 1 || s.Results[0].Action != CloseOnMergeClosed || s.Marked != 1 {
		t.Fatalf("retry sweep = %+v, want closed", s)
	}
}

func TestCloseOnMergeLedgerSweepMergedAfterKeepsFreshMergePrompt(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := newLedgerSweepFixture(t, 7, 8)
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	lastSweep := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	// Issue 8's PR merged before the last full sweep; issue 7's merged after.
	if err := ledger.Record(mergedLedgerClaim(8, lastSweep.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(mergedLedgerClaim(7, lastSweep.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	opts := CloseOnMergeOptions{
		Identity:    HiveIdentity{AppLogin: "hive[bot]"},
		MergedAfter: lastSweep,
		Now:         func() time.Time { return lastSweep.Add(2 * time.Minute) },
	}
	s := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(s.Results) != 1 || s.Results[0].Issue != 7 || s.Results[0].Action != CloseOnMergeClosed {
		t.Fatalf("fresh sweep = %+v, want only issue 7 closed", s)
	}
	if s.SkippedNotFresh != 1 {
		t.Fatalf("skipped not fresh = %d, want 1", s.SkippedNotFresh)
	}
	// One claim read plus CloseIssue's re-read; nothing for issue 8.
	if got := f.count("issue_get"); got != 2 {
		t.Fatalf("issue GETs = %d, want 2", got)
	}
	// The full sweep still reaches the older claim.
	opts.MergedAfter = time.Time{}
	s = c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(s.Results) != 1 || s.Results[0].Issue != 8 || s.SkippedSettled != 1 {
		t.Fatalf("full sweep = %+v, want issue 8 checked and 7 skipped as settled", s)
	}
}

func TestCloseOnMergeResultSettles(t *testing.T) {
	for _, tc := range []struct {
		res  CloseOnMergeResult
		want bool
	}{
		{CloseOnMergeResult{Action: CloseOnMergeClosed, Reason: "closed_completed"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeAlreadyDone, Reason: "issue_not_open"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeAlreadyDone, Reason: "marker_present"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeAwaiting, Reason: "needs_confirmation_or_human"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeSkipped, Reason: "untrusted_author"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeSkipped, Reason: "reopened_after_merge"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeSkipped, Reason: "issue_not_found"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeNoop, Reason: "pr_not_merged"}, true},
		{CloseOnMergeResult{Action: CloseOnMergeSkipped, Reason: "timeline_read_failed"}, false},
		{CloseOnMergeResult{Action: CloseOnMergeNoop, Reason: "issue_read_failed"}, false},
		{CloseOnMergeResult{Action: CloseOnMergeNoop, Reason: "awaiting_comment_failed"}, false},
		{CloseOnMergeResult{Action: CloseOnMergeNoop, Reason: "close_comment_failed"}, false},
		{CloseOnMergeResult{Action: CloseOnMergeNoop, Reason: "close_failed"}, false},
		{CloseOnMergeResult{}, false},
		{CloseOnMergeResult{Action: CloseOnMergeAction("other")}, false},
	} {
		if got := tc.res.Settles(); got != tc.want {
			t.Errorf("%s/%s settles = %v, want %v", tc.res.Action, tc.res.Reason, got, tc.want)
		}
	}
}

func TestClaimLedgerMarkCloseOnMergeSettled(t *testing.T) {
	var nilLedger *ClaimLedger
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	if nilLedger.MarkCloseOnMergeSettled("o/r", 7, 42, at) {
		t.Fatal("nil ledger marked")
	}
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	if err := ledger.Record(IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 42, PRRepo: "o/r", MergedPR: true}); err != nil {
		t.Fatal(err)
	}
	if ledger.MarkCloseOnMergeSettled("o/r", 7, 43, at) {
		t.Fatal("marked a claim now owned by a different PR")
	}
	if ledger.MarkCloseOnMergeSettled("o/r", 99, 42, at) {
		t.Fatal("marked an unknown claim")
	}
	if !ledger.MarkCloseOnMergeSettled("o/r", 7, 42, at) {
		t.Fatal("did not mark the matching claim")
	}
	claim, _ := ledger.Lookup("o/r", 7)
	if !claim.CloseOnMergeSettledAt.Equal(at) {
		t.Fatalf("settled at = %v, want %v", claim.CloseOnMergeSettledAt, at)
	}
}

// The authoritative reconcile replaces the claim map from the live scan; the
// settled marker must ride along while the same PR keeps claiming the issue
// and reset when a different PR takes over.
func TestClaimLedgerReconcileCarriesCloseOnMergeSettled(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	ledger.SetClock(func() time.Time { return now })
	base := IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 42, PRRepo: "o/r", MergedPR: true, MergedAt: now.Add(-time.Hour), ObservedAt: now}
	ledger.Reconcile([]IssueClaim{base}, true)
	if !ledger.MarkCloseOnMergeSettled("o/r", 7, 42, now) {
		t.Fatal("mark failed")
	}
	// Same PR re-listed by the scan: marker carried.
	ledger.Reconcile([]IssueClaim{base}, true)
	claim, _ := ledger.Lookup("o/r", 7)
	if !claim.CloseOnMergeSettledAt.Equal(now) {
		t.Fatalf("settled marker lost on same-PR reconcile: %v", claim.CloseOnMergeSettledAt)
	}
	// Non-authoritative refresh: carried too.
	ledger.Reconcile([]IssueClaim{base}, false)
	claim, _ = ledger.Lookup("o/r", 7)
	if !claim.CloseOnMergeSettledAt.Equal(now) {
		t.Fatalf("settled marker lost on non-authoritative reconcile: %v", claim.CloseOnMergeSettledAt)
	}
	// A new open PR taking over the issue (it outranks the merged claim)
	// starts unsettled.
	other := base
	other.PRNumber = 43
	other.MergedPR = false
	other.MergedAt = time.Time{}
	ledger.Reconcile([]IssueClaim{other}, true)
	claim, _ = ledger.Lookup("o/r", 7)
	if claim.PRNumber != 43 || !claim.CloseOnMergeSettledAt.IsZero() {
		t.Fatalf("new PR inherited marker: %+v", claim)
	}
}

func TestCloseOnMergeClaimHelpers(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	if closeOnMergeClaimSettled(IssueClaim{}, now, time.Hour) {
		t.Fatal("zero marker counted as settled")
	}
	if !closeOnMergeClaimSettled(IssueClaim{CloseOnMergeSettledAt: now.Add(-time.Minute)}, now, time.Hour) {
		t.Fatal("recent marker not settled")
	}
	if closeOnMergeClaimSettled(IssueClaim{CloseOnMergeSettledAt: now.Add(-2 * time.Hour)}, now, time.Hour) {
		t.Fatal("expired marker still settled")
	}
	a, b, c := now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)
	if got := closeOnMergeClaimMergedAt(IssueClaim{MergedAt: a, FirstObservedAt: b, ObservedAt: c}); !got.Equal(a) {
		t.Fatalf("merged at = %v, want MergedAt", got)
	}
	if got := closeOnMergeClaimMergedAt(IssueClaim{FirstObservedAt: b, ObservedAt: c}); !got.Equal(b) {
		t.Fatalf("merged at = %v, want FirstObservedAt", got)
	}
	if got := closeOnMergeClaimMergedAt(IssueClaim{ObservedAt: c}); !got.Equal(c) {
		t.Fatalf("merged at = %v, want ObservedAt", got)
	}
	var o CloseOnMergeOptions
	if o.recheckAfter() != DefaultCloseOnMergeRecheck {
		t.Fatal("zero recheck not defaulted")
	}
	if o.now().IsZero() {
		t.Fatal("nil clock returned zero")
	}
}
