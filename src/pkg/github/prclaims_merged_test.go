package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		"user":       map[string]any{"login": author},
		"html_url":   fmt.Sprintf("https://github.com/torch-spyre/spyre-inference/pull/%d", number),
		"updated_at": updatedAt.Format(time.RFC3339),
	}
	if !mergedAt.IsZero() {
		m["merged_at"] = mergedAt.Format(time.RFC3339)
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
// merged claims (#6867): a merged strong claim suppresses and never takes the
// red+stale release valve (the PR is merged — check state is meaningless), and
// a merged weak claim defers until weakClaimDeferWindow past the merge, then
// releases the remainder.
func TestFilterClaimedIssuesMergedClaims(t *testing.T) {
	mkResult := func() *ActionableResult {
		r := &ActionableResult{}
		r.Issues.Items = []Issue{{Repo: "spyre-inference", Number: 300, Title: "settled"}}
		r.Issues.Count = 1
		return r
	}
	alwaysRedStale := func(string, int) bool { return true }

	t.Run("merged strong claim suppresses despite red+stale", func(t *testing.T) {
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 500,
			MergedPR: true, MergedAt: time.Now().Add(-time.Hour),
			ObservedAt: time.Now(), FirstObservedAt: time.Now().Add(-time.Hour),
		}}, true)
		result := mkResult()
		if got := FilterClaimedIssues(result, ledger, alwaysRedStale, testLogger()); got != 1 {
			t.Fatalf("suppressed = %d, want 1", got)
		}
		if len(result.Issues.Items) != 0 {
			t.Errorf("issue not suppressed: %+v", result.Issues.Items)
		}
	})

	t.Run("merged weak claim defers within the window", func(t *testing.T) {
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 501, Reference: true,
			MergedPR: true, MergedAt: time.Now().Add(-time.Hour),
			ObservedAt: time.Now(), FirstObservedAt: time.Now().Add(-time.Hour),
		}}, true)
		result := mkResult()
		if got := FilterClaimedIssues(result, ledger, alwaysRedStale, testLogger()); got != 1 {
			t.Fatalf("suppressed = %d, want 1 — the red+stale valve must not release a merged claim", got)
		}
	})

	t.Run("merged weak claim releases past the window", func(t *testing.T) {
		mergedAt := time.Now().Add(-weakClaimDeferWindow - time.Hour)
		ledger := NewClaimLedger("", testLogger())
		ledger.Reconcile([]IssueClaim{{
			Repo: "spyre-inference", Issue: 300, PRNumber: 501, Reference: true,
			MergedPR: true, MergedAt: mergedAt,
			ObservedAt: time.Now(), FirstObservedAt: mergedAt,
		}}, true)
		result := mkResult()
		if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 0 {
			t.Fatalf("suppressed = %d, want 0 — a merged Refs must release the remainder past the window", got)
		}
		if len(result.Issues.Items) != 1 {
			t.Errorf("issue wrongly suppressed")
		}
	})
}
