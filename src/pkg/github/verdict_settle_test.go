package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#7871 suite: a no_work_needed reason's citation becomes a
// verified ledger claim, and the ledger keeps it across authoritative scans
// that cannot see the settling PR.

func TestParseSettlingRefs_IncidentRows(t *testing.T) {
	const repo = "projectbluefin/actions"
	cases := []struct {
		name   string
		reason string
		issue  int
		want   []string
	}{
		{
			name:   "actions#533 via merged #532, issue itself excluded",
			reason: "all four workflow entries #533 asks for are already merged into upstream/main's unit-tests.yml paths filters (via #532)",
			issue:  533,
			want:   []string{repo + "#532"},
		},
		{
			name:   "testsuite#866 via 'merged PR #867' with a date that is not a SHA",
			reason: "e2e.yml already has migration-target/extra-tags restored via merged PR #867 (2026-09-20)",
			issue:  866,
			want:   []string{repo + "#867"},
		},
		{
			name:   "utah#18 direct commit",
			reason: "…already merged in upstream/main (e6d3de3)",
			issue:  18,
			want:   []string{repo + "@e6d3de3"},
		},
		{
			name:   "testsuite#794 covered by open PR that resolves two issues",
			reason: `already fully covered by open PR #808 … explicitly "Resolves #793 and #794"`,
			issue:  794,
			want:   []string{repo + "#808", repo + "#793"},
		},
		{
			name:   "pull URL, qualified ref and bare ref de-duplicate",
			reason: "fixed by https://github.com/projectbluefin/actions/pull/532/files and projectbluefin/actions#532 (#532); see other/repo#9",
			issue:  533,
			want:   []string{repo + "#532", "other/repo#9"},
		},
		{
			name:   "digits-only tokens are never SHAs",
			reason: "port 8080 and 1234567 are not commits; 2026092000 either",
			issue:  1,
			want:   nil,
		},
		{
			name:   "empty reason",
			reason: "   ",
			issue:  1,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSettlingRefs(tc.reason, repo, tc.issue)
			var gotS []string
			for _, r := range got {
				gotS = append(gotS, r.String())
			}
			if strings.Join(gotS, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ParseSettlingRefs(%q) = %v, want %v", tc.reason, gotS, tc.want)
			}
		})
	}
}

func TestParseSettlingRefs_CapsFanOut(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "#%d ", 100+i)
	}
	got := ParseSettlingRefs(b.String(), "o/r", 1)
	if len(got) != maxSettlingRefs {
		t.Fatalf("expected cap of %d refs, got %d", maxSettlingRefs, len(got))
	}
}

// settleMux serves the endpoints VerifySettlingRef touches.
func settleMux(t *testing.T, routes map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, body := range routes {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if code, ok := body.(int); ok {
				http.Error(w, `{"message":"boom"}`, code)
				return
			}
			_ = json.NewEncoder(w).Encode(body)
		})
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func settlePR(number int, state string, merged bool, mergedAt time.Time, author string) map[string]any {
	pr := map[string]any{
		"number":   number,
		"state":    state,
		"merged":   merged,
		"html_url": fmt.Sprintf("https://github.com/hivecommons/hive/pull/%d", number),
		"user":     map[string]any{"login": author},
		"base":     map[string]any{"repo": map[string]any{"full_name": "hivecommons/hive"}},
	}
	if merged {
		pr["merged_at"] = mergedAt.UTC().Format(time.RFC3339)
	}
	return pr
}

func TestVerifySettlingRef_MergedBeforeDispatchIsStrongMergedClaim(t *testing.T) {
	dispatched := time.Now()
	mergedAt := dispatched.Add(-2 * time.Hour)
	ts := settleMux(t, map[string]any{
		"/repos/hivecommons/hive/pulls/532": settlePR(532, "closed", true, mergedAt, "bob"),
	})
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res, err := c.VerifySettlingRef(context.Background(), "hivecommons/hive", 533, SettlingRef{Repo: "hivecommons/hive", Number: 532}, dispatched)
	if err != nil || !res.Settled {
		t.Fatalf("expected settled, got %+v err=%v", res, err)
	}
	cl := res.Claim
	if !cl.MergedPR || cl.Reference || cl.ExternalAuthor || cl.PRNumber != 532 || cl.Issue != 533 ||
		cl.Repo != "hivecommons/hive" || cl.PRAuthor != "bob" || cl.Source != ClaimSourceVerdict ||
		!cl.MergedAt.Equal(mergedAt.Truncate(time.Second)) {
		t.Fatalf("unexpected claim: %+v", cl)
	}
}

func TestVerifySettlingRef_MergedAfterDispatchIsRejected(t *testing.T) {
	dispatched := time.Now().Add(-time.Hour)
	ts := settleMux(t, map[string]any{
		"/repos/hivecommons/hive/pulls/532": settlePR(532, "closed", true, time.Now(), "bob"),
	})
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())
	res, err := c.VerifySettlingRef(context.Background(), "hivecommons/hive", 533, SettlingRef{Repo: "hivecommons/hive", Number: 532}, dispatched)
	if err != nil || res.Settled || !strings.Contains(res.Reason, "after the task was dispatched") {
		t.Fatalf("expected clean negative, got %+v err=%v", res, err)
	}
}

func TestVerifySettlingRef_OpenPRIsWeakExternalClaim(t *testing.T) {
	ts := settleMux(t, map[string]any{
		"/repos/hivecommons/hive/pulls/808": settlePR(808, "open", false, time.Time{}, "carol"),
	})
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())
	res, err := c.VerifySettlingRef(context.Background(), "hivecommons/hive", 794, SettlingRef{Repo: "hivecommons/hive", Number: 808}, time.Now())
	if err != nil || !res.Settled {
		t.Fatalf("expected settled, got %+v err=%v", res, err)
	}
	if res.Claim.MergedPR || !res.Claim.Reference || !res.Claim.ExternalAuthor || res.Claim.PRAuthor != "carol" {
		t.Fatalf("expected weak external open claim, got %+v", res.Claim)
	}
}

func TestVerifySettlingRef_ClosedUnmergedAndForeignRepoAndAPIError(t *testing.T) {
	ts := settleMux(t, map[string]any{
		"/repos/hivecommons/hive/pulls/1":   settlePR(1, "closed", false, time.Time{}, "dave"),
		"/repos/hivecommons/hive/pulls/500": http.StatusInternalServerError,
	})
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())
	ctx := context.Background()

	if res, err := c.VerifySettlingRef(ctx, "hivecommons/hive", 2, SettlingRef{Repo: "hivecommons/hive", Number: 1}, time.Now()); err != nil || res.Settled {
		t.Fatalf("closed-unmerged must be a clean negative: %+v err=%v", res, err)
	}
	if res, err := c.VerifySettlingRef(ctx, "hivecommons/hive", 2, SettlingRef{Repo: "other/repo", Number: 1}, time.Now()); err != nil || res.Settled || !strings.Contains(res.Reason, "not in task repo") {
		t.Fatalf("foreign repo must be a clean negative without an API call: %+v err=%v", res, err)
	}
	if res, err := c.VerifySettlingRef(ctx, "hivecommons/hive", 2, SettlingRef{Repo: "hivecommons/hive", Number: 500}, time.Now()); err == nil || res.Settled {
		t.Fatalf("API failure must return the error and Settled=false: %+v err=%v", res, err)
	}
	var nilClient *Client
	if res, err := nilClient.VerifySettlingRef(ctx, "hivecommons/hive", 2, SettlingRef{Repo: "hivecommons/hive", Number: 1}, time.Now()); err == nil || res.Settled {
		t.Fatalf("nil client must fail closed: %+v err=%v", res, err)
	}
}

func TestVerifySettlingRef_CommitOnDefaultBranchPrefersAssociatedMergedPR(t *testing.T) {
	dispatched := time.Now()
	committed := dispatched.Add(-3 * time.Hour)
	ts := settleMux(t, map[string]any{
		"/repos/hivecommons/hive": map[string]any{"default_branch": "main"},
		"/repos/hivecommons/hive/compare/main...e6d3de3": map[string]any{
			"status": "behind",
		},
		"/repos/hivecommons/hive/commits/e6d3de3": map[string]any{
			"sha": "e6d3de3aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"commit": map[string]any{
				"committer": map[string]any{"date": committed.UTC().Format(time.RFC3339)},
			},
		},
		"/repos/hivecommons/hive/commits/e6d3de3aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/pulls": []any{
			settlePR(19, "closed", true, committed, "erin"),
		},
	})
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())
	res, err := c.VerifySettlingRef(context.Background(), "hivecommons/hive", 18, SettlingRef{Repo: "hivecommons/hive", SHA: "e6d3de3"}, dispatched)
	if err != nil || !res.Settled {
		t.Fatalf("expected settled, got %+v err=%v", res, err)
	}
	if !res.Claim.MergedPR || res.Claim.PRNumber != 19 || res.Claim.PRAuthor != "erin" {
		t.Fatalf("expected the associated merged PR to be recorded, got %+v", res.Claim)
	}
}

func TestVerifySettlingRef_CommitWithoutPRRecordsCommitItself(t *testing.T) {
	dispatched := time.Now()
	committed := dispatched.Add(-3 * time.Hour)
	ts := settleMux(t, map[string]any{
		"/repos/hivecommons/hive":                        map[string]any{"default_branch": "main"},
		"/repos/hivecommons/hive/compare/main...abc1234": map[string]any{"status": "identical"},
		"/repos/hivecommons/hive/commits/abc1234": map[string]any{
			"sha":    "abc1234bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"commit": map[string]any{"committer": map[string]any{"date": committed.UTC().Format(time.RFC3339)}},
		},
		"/repos/hivecommons/hive/commits/abc1234/pulls": []any{},
	})
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())
	res, err := c.VerifySettlingRef(context.Background(), "hivecommons/hive", 18, SettlingRef{Repo: "hivecommons/hive", SHA: "abc1234"}, dispatched)
	if err != nil || !res.Settled {
		t.Fatalf("expected settled, got %+v err=%v", res, err)
	}
	if !res.Claim.MergedPR || res.Claim.PRNumber != 0 || !strings.HasSuffix(res.Claim.PRURL, "/commit/abc1234bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") {
		t.Fatalf("expected a commit claim, got %+v", res.Claim)
	}
}

func TestVerifySettlingRef_CommitNotOnDefaultBranchIsRejected(t *testing.T) {
	ts := settleMux(t, map[string]any{
		"/repos/hivecommons/hive":                        map[string]any{"default_branch": "main"},
		"/repos/hivecommons/hive/compare/main...abc1234": map[string]any{"status": "ahead"},
	})
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())
	res, err := c.VerifySettlingRef(context.Background(), "hivecommons/hive", 18, SettlingRef{Repo: "hivecommons/hive", SHA: "abc1234"}, time.Now())
	if err != nil || res.Settled || !strings.Contains(res.Reason, "not reachable") {
		t.Fatalf("expected clean negative, got %+v err=%v", res, err)
	}
}

func TestClaimLedger_RecordPersistsAndLookupSees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	l := NewClaimLedger(path, testLogger())
	c := IssueClaim{Repo: "o/r", Issue: 533, PRNumber: 532, PRRepo: "o/r", PRURL: "u", MergedPR: true, Source: ClaimSourceVerdict}
	if err := l.Record(c); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, ok := l.Lookup("o/r", 533)
	if !ok || got.PRNumber != 532 || got.ObservedAt.IsZero() || got.FirstObservedAt.IsZero() {
		t.Fatalf("Lookup after Record = %+v, %v", got, ok)
	}
	reloaded, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, ok := reloaded.Lookup("o/r", 533); !ok || got.Source != ClaimSourceVerdict {
		t.Fatalf("Record did not persist: %+v, %v", got, ok)
	}
	if err := l.Record(IssueClaim{Repo: "o/r"}); err != nil || l.Len() != 1 {
		t.Fatalf("a claim with no issue must be ignored: err=%v len=%d", err, l.Len())
	}
}

func TestClaimLedger_AuthoritativeReconcileCarriesVerdictClaimsUntilTTL(t *testing.T) {
	l := NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), testLogger())
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return now })

	verdict := IssueClaim{Repo: "o/r", Issue: 533, PRNumber: 532, PRRepo: "o/r", MergedPR: true, Source: ClaimSourceVerdict, ObservedAt: now}
	scanned := IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 8, PRRepo: "o/r", ObservedAt: now}
	if err := l.Record(verdict); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(scanned); err != nil {
		t.Fatal(err)
	}

	// The scan that follows sees neither PR (the settling one never
	// referenced the issue; the other closed): the verdict claim survives,
	// the scan claim is dropped as before.
	l.Reconcile(nil, true)
	if _, ok := l.Lookup("o/r", 533); !ok {
		t.Fatal("verdict claim must survive an authoritative reconcile that cannot see its PR")
	}
	if _, ok := l.Lookup("o/r", 7); ok {
		t.Fatal("a scan claim whose PR vanished must still be dropped")
	}

	// A live OPEN strong claim for the same issue outranks the merged
	// verdict claim (#6867 rank rule) and replaces it.
	live := IssueClaim{Repo: "o/r", Issue: 533, PRNumber: 900, PRRepo: "o/r", ObservedAt: now}
	l.Reconcile([]IssueClaim{live}, true)
	if got, _ := l.Lookup("o/r", 533); got.PRNumber != 900 {
		t.Fatalf("live open claim must win over the carried verdict claim, got %+v", got)
	}
	// Once the live PR closes, nothing is carried: the verdict claim was
	// displaced, not shelved.
	l.Reconcile(nil, true)
	if _, ok := l.Lookup("o/r", 533); ok {
		t.Fatal("displaced verdict claim must not resurrect")
	}

	// #8003: the 72h ledger TTL no longer retires a carried verdict claim —
	// the settling PR is still merged at 72h, so the clock alone was
	// re-offering the issue every three days. Only settledClaimRetention,
	// measured from the verdict, does.
	if err := l.Record(verdict); err != nil {
		t.Fatal(err)
	}
	now = now.Add(claimLedgerTTL + time.Minute)
	l.Reconcile(nil, true)
	if _, ok := l.Lookup("o/r", 533); !ok {
		t.Fatal("verdict claim past the 72h TTL must still be carried (#8003)")
	}
	now = now.Add(settledClaimRetention)
	l.Reconcile(nil, true)
	if _, ok := l.Lookup("o/r", 533); ok {
		t.Fatal("verdict claim past settledClaimRetention must be retired")
	}
}
