package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// onGitHubExceptLab is a RepoOnGitHub predicate for a hive whose "lab" repo
// lives on GitLab.
func onGitHubExceptLab(repo string) bool { return !strings.HasSuffix(repo, "lab") }

// A ledger claim whose issue or PR repo lives on another forge is skipped
// without a GitHub API call: looking it up under its slug would read (and
// could close) an unrelated GitHub repo of the same name (#14150).
func TestCloseOnMergeLedgerSweepSkipsOtherForgeClaims(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := newLedgerSweepFixture(t, 7)
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	merged := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	onLab := mergedLedgerClaim(8, merged)
	onLab.Repo, onLab.PRRepo = "o/lab", "o/lab"
	prOnLab := mergedLedgerClaim(9, merged)
	prOnLab.PRRepo = "o/lab"
	for _, claim := range []IssueClaim{mergedLedgerClaim(7, merged), onLab, prOnLab} {
		if err := ledger.Record(claim); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestClient(t, srv, "o", []string{"r", "lab"})
	now := merged.Add(time.Hour)
	opts := CloseOnMergeOptions{
		Identity:     HiveIdentity{AppLogin: "hive[bot]"},
		Now:          func() time.Time { return now },
		RepoOnGitHub: onGitHubExceptLab,
	}

	sweep := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if sweep.SkippedOtherForge != 2 {
		t.Fatalf("SkippedOtherForge = %d, want 2 (issue on lab, PR on lab)", sweep.SkippedOtherForge)
	}
	if len(sweep.Results) != 1 || sweep.Results[0].Issue != 7 || sweep.Results[0].Action != CloseOnMergeClosed {
		t.Fatalf("results = %+v, want only issue 7 closed", sweep.Results)
	}
	if got := f.count("pull_get"); got != 1 {
		t.Fatalf("PR GETs = %d, want 1 (the GitHub claim only)", got)
	}
	for _, claim := range ledger.Claims() {
		if claim.Issue != 7 && !claim.CloseOnMergeSettledAt.IsZero() {
			t.Fatalf("other-forge claim %d was settled; it must stay for a forge-aware sweep", claim.Issue)
		}
	}
}

// The backfill lists open issues per repo through the GitHub API; a repo on
// another forge is never listed.
func TestCloseOnMergeBackfillSkipsOtherForgeRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub request for an other-forge repo: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"lab"})
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}, RepoOnGitHub: onGitHubExceptLab}
	if results := c.CloseOnMergeBackfill(context.Background(), opts); len(results) != 0 {
		t.Fatalf("results = %+v, want none", results)
	}
}

// The webhook path restricts the ledger sweep to the PR that just merged:
// the other merged claims cost no API call and stay unsettled for the
// scheduled sweep.
func TestCloseOnMergeLedgerSweepOnlyPRFilter(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := newLedgerSweepFixture(t, 7, 8)
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	merged := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, issue := range []int{7, 8} {
		if err := ledger.Record(mergedLedgerClaim(issue, merged)); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	opts := CloseOnMergeOptions{
		Identity:     HiveIdentity{AppLogin: "hive[bot]"},
		Now:          func() time.Time { return merged.Add(time.Hour) },
		OnlyPRRepo:   "r",
		OnlyPRNumber: 8,
	}

	sweep := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(sweep.Results) != 1 || sweep.Results[0].Issue != 8 || sweep.Results[0].Action != CloseOnMergeClosed {
		t.Fatalf("results = %+v, want only issue 8 closed", sweep.Results)
	}
	if got := f.count("pull_get"); got != 1 {
		t.Fatalf("PR GETs = %d, want 1", got)
	}
	for _, claim := range ledger.Claims() {
		if settled := !claim.CloseOnMergeSettledAt.IsZero(); settled != (claim.Issue == 8) {
			t.Fatalf("claim %d settled=%v", claim.Issue, settled)
		}
	}
}
