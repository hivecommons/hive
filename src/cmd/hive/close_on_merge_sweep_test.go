package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Tests for the close-on-merge sweep scheduling in cmd/hive: the full-ledger
// walk runs on issues.close_on_merge_interval, passes in between only look at
// fresh merges, and everything pauses while the API budget is constrained.
// The per-claim verdicts and the ledger's settled marker are covered in
// pkg/github.

func TestCloseOnMergeSchedulerPlan(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	sched := newCloseOnMergeScheduler(func() time.Time { return now })
	const sweepEvery, backfillEvery = 10 * time.Minute, time.Hour

	first := sched.plan(github.APIBudgetNormal, sweepEvery, backfillEvery)
	if first.Skip || !first.FullSweep || !first.Backfill || !first.MergedAfter.IsZero() || first.BudgetChanged {
		t.Fatalf("first plan = %+v, want full sweep + backfill", first)
	}

	now = now.Add(2 * time.Minute)
	between := sched.plan(github.APIBudgetNormal, sweepEvery, backfillEvery)
	if between.Skip || between.FullSweep || between.Backfill || !between.MergedAfter.Equal(now.Add(-2*time.Minute)) {
		t.Fatalf("between plan = %+v, want fresh-only with MergedAfter = last sweep", between)
	}

	now = now.Add(8 * time.Minute)
	due := sched.plan(github.APIBudgetNormal, sweepEvery, backfillEvery)
	if !due.FullSweep || due.Backfill {
		t.Fatalf("due plan = %+v, want full sweep, no backfill yet", due)
	}

	now = now.Add(50 * time.Minute)
	backfill := sched.plan(github.APIBudgetNormal, sweepEvery, backfillEvery)
	if !backfill.FullSweep || !backfill.Backfill {
		t.Fatalf("backfill plan = %+v, want both due", backfill)
	}
}

func TestCloseOnMergeSchedulerBudgetTransitionsLogOnce(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	sched := newCloseOnMergeScheduler(func() time.Time { return now })
	const every = 10 * time.Minute

	held := sched.plan(github.APIBudgetConserve, every, every)
	if !held.Skip || !held.BudgetChanged || held.Mode != github.APIBudgetConserve {
		t.Fatalf("first constrained plan = %+v, want skip with transition", held)
	}
	now = now.Add(time.Minute)
	stillHeld := sched.plan(github.APIBudgetCritical, every, every)
	if !stillHeld.Skip || stillHeld.BudgetChanged {
		t.Fatalf("second constrained plan = %+v, want silent skip", stillHeld)
	}
	// A skipped pass must not advance the sweep clock: the first normal pass
	// after the hold runs the full sweep.
	now = now.Add(time.Minute)
	resumed := sched.plan(github.APIBudgetNormal, every, every)
	if resumed.Skip || !resumed.BudgetChanged || !resumed.FullSweep {
		t.Fatalf("resumed plan = %+v, want full sweep with transition", resumed)
	}
	now = now.Add(time.Minute)
	steady := sched.plan(github.APIBudgetNormal, every, every)
	if steady.BudgetChanged || steady.FullSweep {
		t.Fatalf("steady plan = %+v, want quiet fresh-only pass", steady)
	}
}

// newCloseOnMergeAPI serves one open issue o/r#7 fixed by merged PR o/r#42
// and counts every request.
func newCloseOnMergeAPI(t *testing.T, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix", "body": "Fixes #7", "merged": true,
				"merged_at": "2026-10-14T08:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/fix-7", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "body": "body", "labels": []map[string]string{}})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"), r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/timeline"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func closeOnMergeTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Project.AIAuthor = "hive[bot]"
	cfg.GitHub.AppSlug = "hive"
	return cfg
}

func TestRunCloseOnMergeSweepBudgetSkipAndInterval(t *testing.T) {
	var requests atomic.Int64
	srv := newCloseOnMergeAPI(t, &requests)
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "o", []string{"r"}, discardLogger())
	ledger := github.NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	merged := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	if err := ledger.Record(github.IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 42, PRRepo: "o/r", MergedPR: true, MergedAt: merged, FirstObservedAt: merged, ObservedAt: merged}); err != nil {
		t.Fatal(err)
	}

	now := merged.Add(time.Minute)
	mode := github.APIBudgetCritical
	sched := newCloseOnMergeScheduler(func() time.Time { return now })
	sched.budgetMode = func(*github.Client) github.APIBudgetMode { return mode }
	var logBuf strings.Builder
	logger := sweepTestLogger(&logBuf)
	cfg := closeOnMergeTestConfig()

	// Budget constrained: no API call, one log line, repeated passes silent.
	runCloseOnMergeSweepWith(context.Background(), cfg, client, ledger, logger, sched)
	runCloseOnMergeSweepWith(context.Background(), cfg, client, ledger, logger, sched)
	if requests.Load() != 0 {
		t.Fatalf("constrained budget made %d API calls", requests.Load())
	}
	if n := strings.Count(logBuf.String(), "sweeps paused"); n != 1 {
		t.Fatalf("paused log lines = %d, want 1: %s", n, logBuf.String())
	}

	// Budget normal: the full sweep closes the issue and stamps the claim.
	mode = github.APIBudgetNormal
	runCloseOnMergeSweepWith(context.Background(), cfg, client, ledger, logger, sched)
	if !strings.Contains(logBuf.String(), "sweeps resumed") {
		t.Fatalf("resume not logged: %s", logBuf.String())
	}
	after := requests.Load()
	if after == 0 {
		t.Fatal("normal budget made no API calls")
	}
	claim, ok := ledger.Lookup("o/r", 7)
	if !ok || !claim.CloseOnMergeSettledAt.Equal(now) {
		t.Fatalf("claim not settled: %+v", claim)
	}

	// Next pass inside the interval: fresh-only, and the settled claim costs
	// nothing.
	now = now.Add(time.Minute)
	runCloseOnMergeSweepWith(context.Background(), cfg, client, ledger, logger, sched)
	if requests.Load() != after {
		t.Fatalf("between-sweep pass made %d API calls", requests.Load()-after)
	}
}

func TestRunCloseOnMergeSweepFreshMergeClosesBetweenSweeps(t *testing.T) {
	var requests atomic.Int64
	srv := newCloseOnMergeAPI(t, &requests)
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "o", []string{"r"}, discardLogger())
	ledger := github.NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)

	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	sched := newCloseOnMergeScheduler(func() time.Time { return now })
	sched.budgetMode = func(*github.Client) github.APIBudgetMode { return github.APIBudgetNormal }
	cfg := closeOnMergeTestConfig()

	// Full sweep over an empty ledger.
	runCloseOnMergeSweepWith(context.Background(), cfg, client, ledger, discardLogger(), sched)
	// A PR merges after that sweep and the scan records its claim.
	merged := now.Add(30 * time.Second)
	if err := ledger.Record(github.IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 42, PRRepo: "o/r", MergedPR: true, MergedAt: merged, FirstObservedAt: merged, ObservedAt: merged}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	runCloseOnMergeSweepWith(context.Background(), cfg, client, ledger, discardLogger(), sched)
	claim, _ := ledger.Lookup("o/r", 7)
	if !claim.CloseOnMergeSettledAt.Equal(now) {
		t.Fatalf("fresh merge not closed between sweeps: %+v", claim)
	}
	if requests.Load() == 0 {
		t.Fatal("fresh-merge pass made no API calls")
	}
}

func TestRunCloseOnMergeSweepDisabledAndNilGuards(t *testing.T) {
	var requests atomic.Int64
	srv := newCloseOnMergeAPI(t, &requests)
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "o", []string{"r"}, discardLogger())
	ledger := github.NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	sched := newCloseOnMergeScheduler(nil)
	cfg := closeOnMergeTestConfig()
	off := false
	cfg.Issues.CloseOnMerge = &off
	runCloseOnMergeSweepWith(context.Background(), cfg, client, ledger, discardLogger(), sched)
	runCloseOnMergeSweepWith(context.Background(), nil, client, ledger, discardLogger(), sched)
	runCloseOnMergeSweepWith(context.Background(), closeOnMergeTestConfig(), nil, ledger, discardLogger(), sched)
	runCloseOnMergeSweepWith(context.Background(), closeOnMergeTestConfig(), client, nil, discardLogger(), sched)
	runCloseOnMergeSweepWith(context.Background(), closeOnMergeTestConfig(), client, ledger, discardLogger(), nil)
	if requests.Load() != 0 {
		t.Fatalf("guarded calls made %d API requests", requests.Load())
	}
	if sched.clock() == nil || sched.mode(nil) != github.APIBudgetNormal {
		t.Fatal("scheduler defaults")
	}
}
