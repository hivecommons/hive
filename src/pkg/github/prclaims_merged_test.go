package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// mergedClaimServer stands up a fake GitHub API for one repo that serves
// different PR lists for the open scan (state=open) and the merged settle
// scan (state=closed), so tests can exercise both halves of FetchClaims
// (kubestellar/hive#6867).
func mergedClaimServer(t *testing.T, open, closed []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("state") == "closed" {
			_ = json.NewEncoder(w).Encode(closed)
			return
		}
		_ = json.NewEncoder(w).Encode(open)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mergedPR builds a closed PR fixture. A zero mergedAt yields a
// closed-without-merge PR.
func mergedPR(number int, author, title, body string, mergedAt, updatedAt time.Time) map[string]any {
	m := map[string]any{
		"number":     number,
		"title":      title,
		"body":       body,
		"state":      "closed",
		"head":       map[string]any{"ref": fmt.Sprintf("merged-pr%dhead", number), "sha": fmt.Sprintf("merged-head-%d", number)},
		"user":       map[string]any{"login": author},
		"html_url":   fmt.Sprintf("https://github.com/torch-spyre/spyre-inference/pull/%d", number),
		"updated_at": updatedAt.Format(time.RFC3339),
	}
	if !mergedAt.IsZero() {
		m["merged_at"] = mergedAt.Format(time.RFC3339)
		m["merged_by"] = map[string]any{"login": author}
	}
	return m
}

// TestFetchClaimsMergedPRs pins the merged-PR settle scan (#6867): a PR that
// merged within mergedClaimScanWindow still claims its issue — marked MergedPR
// and anchored at the merge — while a closed-without-merge PR and a PR merged
// before the window claim nothing.
func TestFetchClaimsMergedPRs(t *testing.T) {
	now := time.Now()
	recent := now.Add(-2 * time.Hour)
	ancient := now.Add(-mergedClaimScanWindow - 24*time.Hour)

	tests := []struct {
		name          string
		closed        []map[string]any
		wantIssues    []int
		wantReference []bool
	}{
		{
			name: "merged PR with closing keyword claims its issue",
			closed: []map[string]any{
				mergedPR(500, "kubestellar-hive[bot]", "fix it", "Fixes #300", recent, recent),
			},
			wantIssues:    []int{300},
			wantReference: []bool{false},
		},
		{
			name: "merged PR with non-closing Refs claims weakly",
			closed: []map[string]any{
				mergedPR(501, "kubestellar-hive[bot]", "part one", "Refs #301", recent, recent),
			},
			wantIssues:    []int{301},
			wantReference: []bool{true},
		},
		{
			name: "closed-without-merge PR claims nothing",
			closed: []map[string]any{
				mergedPR(502, "kubestellar-hive[bot]", "abandoned", "Fixes #302", time.Time{}, recent),
			},
			wantIssues: nil,
		},
		{
			name: "PR merged before the window claims nothing even when updated recently",
			closed: []map[string]any{
				mergedPR(503, "kubestellar-hive[bot]", "old fix", "Fixes #303", ancient, recent),
			},
			wantIssues: nil,
		},
		{
			name: "scan stops at the first PR updated before the window",
			closed: []map[string]any{
				mergedPR(504, "kubestellar-hive[bot]", "stale", "Fixes #304", ancient, ancient),
				// Sorted by updated desc, everything after the cutoff PR is
				// older still — this entry must never be reached.
				mergedPR(505, "kubestellar-hive[bot]", "unreachable", "Fixes #305", recent, ancient),
			},
			wantIssues: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := mergedClaimServer(t, nil, tt.closed)
			c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

			claims, err := c.FetchClaims(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"})
			if err != nil {
				t.Fatalf("FetchClaims: %v", err)
			}
			if len(claims) != len(tt.wantIssues) {
				t.Fatalf("got %+v, want issues %v", claims, tt.wantIssues)
			}
			for i, want := range tt.wantIssues {
				if claims[i].Issue != want {
					t.Errorf("claim[%d].Issue = %d, want %d", i, claims[i].Issue, want)
				}
				if !claims[i].MergedPR {
					t.Errorf("claim[%d].MergedPR = false, want true", i)
				}
				if claims[i].MergedAt.IsZero() {
					t.Errorf("claim[%d].MergedAt is zero", i)
				}
				if claims[i].PRState != PRStateMerged || claims[i].PRHead == "" {
					t.Errorf("claim[%d] missing merged PR state/head: %+v", i, claims[i])
				}
				if !claims[i].FirstObservedAt.Equal(claims[i].MergedAt) {
					t.Errorf("claim[%d].FirstObservedAt = %v, want the merge time %v — the weak deferral window must anchor at the merge",
						i, claims[i].FirstObservedAt, claims[i].MergedAt)
				}
				if i < len(tt.wantReference) && claims[i].Reference != tt.wantReference[i] {
					t.Errorf("claim[%d].Reference = %v, want %v", i, claims[i].Reference, tt.wantReference[i])
				}
			}
		})
	}
}

// TestFetchClaimsMergedAndOpenTogether pins that the two scans compose: the
// open scan's claims are unchanged by the merged scan running after it.
func TestFetchClaimsMergedAndOpenTogether(t *testing.T) {
	now := time.Now()
	srv := mergedClaimServer(t,
		[]map[string]any{pr(423, "kubestellar-hive[bot]", "wip", "Fixes #100")},
		[]map[string]any{mergedPR(500, "kubestellar-hive[bot]", "done", "Fixes #200", now.Add(-time.Hour), now.Add(-time.Hour))},
	)
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	claims, err := c.FetchClaims(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"})
	if err != nil {
		t.Fatalf("FetchClaims: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("got %d claims (%+v), want 2", len(claims), claims)
	}
	if claims[0].Issue != 100 || claims[0].MergedPR {
		t.Errorf("open claim = %+v, want issue 100 with MergedPR=false", claims[0])
	}
	if claims[1].Issue != 200 || !claims[1].MergedPR {
		t.Errorf("merged claim = %+v, want issue 200 with MergedPR=true", claims[1])
	}
}

// TestClaimRankOpenBeatsMergedWithinTier pins the #6867 precedence extension:
// within an evidential tier an open PR outranks a merged one (the live signal
// with a working red+stale valve wins), while across tiers evidence still wins
// — a merged closing keyword outranks an open non-closing reference.
func TestClaimRankOpenBeatsMergedWithinTier(t *testing.T) {
	openStrong := IssueClaim{}
	mergedStrong := IssueClaim{MergedPR: true}
	openWeakRef := IssueClaim{Reference: true}

	if claimRank(openStrong) <= claimRank(mergedStrong) {
		t.Errorf("open strong (%d) must outrank merged strong (%d)",
			claimRank(openStrong), claimRank(mergedStrong))
	}
	if claimRank(mergedStrong) <= claimRank(openWeakRef) {
		t.Errorf("merged strong (%d) must outrank open weak reference (%d) — a settled issue must not be re-offered because a follow-up references it",
			claimRank(mergedStrong), claimRank(openWeakRef))
	}
}

// TestFilterClaimedIssuesMergedClaims pins the agent-side consumption of
// merged claims (#6867/#7061): a merged strong claim suppresses and never takes
// the red+stale release valve (the PR is merged — check state is meaningless),
// while a merged weak claim is released with merged-PR context instead of being
// parked on the 72h timer and later re-offered unchanged.
func TestFilterClaimedIssuesMergedClaims(t *testing.T) {
	mkResult := func() *ActionableResult {
		r := &ActionableResult{}
		r.Issues.Items = []Issue{{Repo: "spyre-inference", Number: 300, Title: "settled"}}
		r.Issues.Count = 1
		return r
	}
	alwaysRedStale := func(string, int) bool { return true }

	t.Run("merged strong claim stays actionable pending confirmation", func(t *testing.T) {
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 500,
			MergedPR: true, MergedAt: time.Now().Add(-time.Hour),
			ObservedAt: time.Now(), FirstObservedAt: time.Now().Add(-time.Hour),
		}}, true)
		result := mkResult()
		if got := FilterClaimedIssues(result, ledger, alwaysRedStale, testLogger()); got != 0 {
			t.Fatalf("suppressed = %d, want 0", got)
		}
		if len(result.Issues.Items) != 1 || !issueHasLabel(result.Issues.Items[0].Labels, LikelyDoneLabel) {
			t.Errorf("issue not kept with likely-done label: %+v", result.Issues.Items)
		}
	})

	t.Run("merged reference-only claim releases immediately with context", func(t *testing.T) {
		mergedAt := time.Now().Add(-time.Hour)
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 501, Reference: true,
			PRRepo: "spyre-inference", PRURL: "https://github.com/torch-spyre/spyre-inference/pull/501",
			MergedPR: true, MergedAt: mergedAt,
			ObservedAt: time.Now(), FirstObservedAt: mergedAt,
		}}, true)
		result := mkResult()
		if got := FilterClaimedIssues(result, ledger, alwaysRedStale, testLogger()); got != 0 {
			t.Fatalf("suppressed = %d, want 0 — a merged Refs claim must not stay on the weak timer", got)
		}
		if len(result.Issues.Items) != 1 {
			t.Fatalf("issue should be actionable with context, got %+v", result.Issues.Items)
		}
		ctx := result.Issues.Items[0].ClaimContext
		if ctx == nil {
			t.Fatal("merged weak claim was released without merged-PR context")
		}
		if ctx.PRNumber != 501 || ctx.PRRepo != "spyre-inference" || !ctx.Reference || !ctx.MergedPR {
			t.Fatalf("claim context = %+v, want merged reference PR #501", ctx)
		}
		if ctx.Decision != "merged_pr_needs_verification" {
			t.Fatalf("decision = %q, want merged_pr_needs_verification", ctx.Decision)
		}
	})

	t.Run("merged weak claim past the old window still carries context", func(t *testing.T) {
		mergedAt := time.Now().Add(-weakClaimDeferWindow - time.Hour)
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 501, Reference: true,
			PRRepo: "spyre-inference", PRURL: "https://github.com/torch-spyre/spyre-inference/pull/501",
			MergedPR: true, MergedAt: mergedAt,
			ObservedAt: time.Now(), FirstObservedAt: mergedAt,
		}}, true)
		result := mkResult()
		if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 0 {
			t.Fatalf("suppressed = %d, want 0 — a merged Refs must not re-enter unchanged past the old window", got)
		}
		if len(result.Issues.Items) != 1 || result.Issues.Items[0].ClaimContext == nil {
			t.Fatalf("issue should be actionable with merged context, got %+v", result.Issues.Items)
		}
	})

	t.Run("merged external-author claim releases with context", func(t *testing.T) {
		mergedAt := time.Now().Add(-time.Hour)
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 777,
			PRRepo: "spyre-inference", PRAuthor: "outside-dev",
			ExternalAuthor: true, MergedPR: true, MergedAt: mergedAt,
			ObservedAt: time.Now(), FirstObservedAt: mergedAt,
		}}, true)
		result := mkResult()
		if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 0 {
			t.Fatalf("suppressed = %d, want 0 for merged external weak claim", got)
		}
		ctx := result.Issues.Items[0].ClaimContext
		if ctx == nil || !ctx.ExternalAuthor || ctx.PRNumber != 777 {
			t.Fatalf("claim context = %+v, want external merged PR #777", ctx)
		}
	})

	t.Run("repeated evaluation is idempotent", func(t *testing.T) {
		mergedAt := time.Now().Add(-time.Hour)
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 501, Reference: true,
			PRRepo: "spyre-inference", PRURL: "https://github.com/torch-spyre/spyre-inference/pull/501",
			MergedPR: true, MergedAt: mergedAt,
			ObservedAt: time.Now(), FirstObservedAt: mergedAt,
		}}, true)
		for i := 0; i < 2; i++ {
			result := mkResult()
			if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 0 {
				t.Fatalf("pass %d suppressed = %d, want 0", i+1, got)
			}
			if len(result.Issues.Items) != 1 || result.Issues.Items[0].ClaimContext == nil {
				t.Fatalf("pass %d released without exactly one contextual issue: %+v", i+1, result.Issues.Items)
			}
		}
	})
}

func TestFilterClaimedIssuesMergedClaimsAreNeverSuppressedByLabels(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	mkResult := func(labels ...string) *ActionableResult {
		return &ActionableResult{Issues: IssueResult{
			Items: []Issue{{Repo: "spyre-inference", Number: 300, Title: "settled", Labels: labels}},
			Count: 1,
		}}
	}
	mkLedger := func() *ClaimLedger {
		ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
		ledger.SetClock(func() time.Time { return now })
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 501,
			PRRepo: "spyre-inference", PRURL: "https://github.com/torch-spyre/spyre-inference/pull/501",
			PRAuthor: "clubanderson", PRState: PRStateMerged, PRHead: "head-a",
			Reference: true, MergedPR: true, MergedAt: now.Add(-time.Hour),
			ObservedAt: now, FirstObservedAt: now.Add(-time.Hour),
		}}, true)
		return ledger
	}

	// hive/likely-done and hive/covered-by-pr are written by SyncIssuePRClaimLabels,
	// not by an agent, so they must never count as a verification outcome: an
	// open issue behind a merged PR stays listed until an agent closes it or
	// labels it hive/verified-open.
	for _, labels := range [][]string{nil, {LikelyDoneLabel}, {CoveredByPRLabel}, {LikelyDoneLabel, CoveredByPRLabel}} {
		ledger := mkLedger()
		for kick := 0; kick < 3; kick++ {
			result := mkResult(labels...)
			if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 0 {
				t.Fatalf("labels=%v kick %d: suppressed = %d, want 0", labels, kick, got)
			}
			if len(result.Issues.Items) != 1 || result.Issues.Items[0].ClaimContext == nil || !result.Issues.Items[0].ClaimContext.MergedPR {
				t.Fatalf("labels=%v kick %d: merged claim should stay listed with context: %+v", labels, kick, result.Issues.Items)
			}
		}
	}

	// hive/verified-open is the agent's outcome that work remains: the issue
	// stays actionable (the kick renders "implement the rest") and the automatic
	// likely-done label is dropped from the rendered labels.
	ledger := mkLedger()
	result := mkResult(VerifiedOpenLabel, LikelyDoneLabel)
	if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 0 {
		t.Fatalf("verified-open suppressed = %d, want 0", got)
	}
	if len(result.Issues.Items) != 1 {
		t.Fatalf("verified-open issue must remain actionable, got %+v", result.Issues.Items)
	}
	got := result.Issues.Items[0]
	if !issueHasLabel(got.Labels, VerifiedOpenLabel) || issueHasLabel(got.Labels, LikelyDoneLabel) {
		t.Fatalf("verified-open issue labels = %v, want verified-open kept and likely-done removed", got.Labels)
	}
	if got.ClaimContext == nil || !got.ClaimContext.MergedPR {
		t.Fatalf("verified-open issue must keep merged claim context, got %+v", got.ClaimContext)
	}
}

func TestFilterClaimedIssuesOpenExternalClaims(t *testing.T) {
	now := time.Now()
	mk := func() (*ActionableResult, *ClaimLedger) {
		result := &ActionableResult{Issues: IssueResult{
			Items: []Issue{{Repo: "spyre-inference", Number: 300, Title: "covered"}},
			Count: 1,
		}}
		ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 777,
			PRRepo: "spyre-inference", PRURL: "https://github.com/torch-spyre/spyre-inference/pull/777",
			PRAuthor: "outside-dev", PRState: PRStateOpen, PRHead: "open-head",
			ExternalAuthor: true, ObservedAt: now, FirstObservedAt: now,
		}}, true)
		return result, ledger
	}

	t.Run("not red-stale suppresses", func(t *testing.T) {
		result, ledger := mk()
		if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 1 {
			t.Fatalf("suppressed = %d, want 1", got)
		}
		if len(result.Issues.Items) != 0 {
			t.Fatalf("open external claim should be removed from kicks, got %+v", result.Issues.Items)
		}
	})

	t.Run("red-stale release valve keeps it actionable", func(t *testing.T) {
		result, ledger := mk()
		redStale := func(repo string, pr int) bool { return repo == "spyre-inference" && pr == 777 }
		if got := FilterClaimedIssues(result, ledger, redStale, testLogger()); got != 0 {
			t.Fatalf("suppressed = %d, want 0", got)
		}
		if len(result.Issues.Items) != 1 {
			t.Fatalf("red-stale external claim should remain actionable, got %+v", result.Issues.Items)
		}
	})
}

func TestFilterClaimedIssuesReferenceClaimStillHonorsRedStale(t *testing.T) {
	now := time.Now()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
	ledger.Reconcile([]IssueClaim{{
		Repo: "spyre-inference", Issue: 300, PRNumber: 778,
		PRRepo: "spyre-inference", PRURL: "https://github.com/torch-spyre/spyre-inference/pull/778",
		PRAuthor: "clubanderson", PRState: PRStateOpen, PRHead: "stale-head",
		Reference: true, ObservedAt: now, FirstObservedAt: now,
	}}, true)
	result := &ActionableResult{Issues: IssueResult{
		Items: []Issue{{Repo: "spyre-inference", Number: 300, Title: "covered"}},
		Count: 1,
	}}
	redStale := func(repo string, pr int) bool { return repo == "spyre-inference" && pr == 778 }
	if got := FilterClaimedIssues(result, ledger, redStale, testLogger()); got != 0 {
		t.Fatalf("suppressed = %d, want 0", got)
	}
	if len(result.Issues.Items) != 1 {
		t.Fatalf("red-stale reference claim should remain actionable, got %+v", result.Issues.Items)
	}
}
