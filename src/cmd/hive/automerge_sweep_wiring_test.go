package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
)

// Tests for the eval-loop auto-merge sweep wiring and the duplicate-PR guard
// wiring in cmd/hive/main.go: runAutoMergeSweepIfDue (throttle, lastRun
// stamping, nil-client no-op, error handling), getClaimLedger (lazy sync.Once
// load, corrupt-ledger fallback), and applyDuplicatePRGuard (end-to-end
// suppression of issues claimed by an open hive-authored PR). The sweep and
// guard DECISIONS (which PRs merge, how claims parse) are covered in
// pkg/github; these tests cover only the main.go wiring around them.

func sweepTestLogger(buf *strings.Builder) *slog.Logger {
	if buf == nil {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(buf, nil))
}

// newSweepAPI serves the two endpoints runAutoMergeSweepIfDue's sweep touches
// for repo testorg/widget: the labelled-issue listing and (optionally) the PR
// fetch. It counts every request so throttle tests can assert "no API call".
func newSweepAPI(t *testing.T, issuesJSON string, issuesStatus int, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if issuesStatus != http.StatusOK {
			w.WriteHeader(issuesStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(issuesJSON))
	})
	mux.HandleFunc("/repos/testorg/widget/pulls/", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		// Fail the PR fetch so the sweep counts the PR as seen+skipped without
		// this test re-driving the whole merge pipeline (covered in pkg/github).
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunAutoMergeSweepIfDue_NilClientIsNoop(t *testing.T) {
	var lastRun time.Time
	runAutoMergeSweepIfDue(context.Background(), nil, nil, &lastRun, sweepTestLogger(nil))
	if !lastRun.IsZero() {
		t.Fatalf("nil client must not stamp lastRun, got %v", lastRun)
	}
}

func TestRunAutoMergeSweepIfDue_ThrottledWithinInterval(t *testing.T) {
	var requests atomic.Int64
	srv := newSweepAPI(t, `[]`, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	lastRun := time.Now()
	before := lastRun
	runAutoMergeSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(nil))

	if got := requests.Load(); got != 0 {
		t.Fatalf("throttled sweep must make no API calls, got %d", got)
	}
	if !lastRun.Equal(before) {
		t.Fatalf("throttled sweep must not advance lastRun: before=%v after=%v", before, lastRun)
	}
}

func TestRunAutoMergeSweepIfDue_DueSweepStampsLastRunAndCallsAPI(t *testing.T) {
	var requests atomic.Int64
	srv := newSweepAPI(t, `[]`, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var lastRun time.Time
	start := time.Now()
	runAutoMergeSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(nil))

	if requests.Load() == 0 {
		t.Fatal("due sweep made no API calls")
	}
	if lastRun.Before(start) {
		t.Fatalf("due sweep must stamp lastRun, got %v", lastRun)
	}
}

func TestRunAutoMergeSweepIfDue_NilLastRunStillSweeps(t *testing.T) {
	var requests atomic.Int64
	srv := newSweepAPI(t, `[]`, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	runAutoMergeSweepIfDue(context.Background(), ghClient, nil, nil, sweepTestLogger(nil))

	if requests.Load() == 0 {
		t.Fatal("sweep with nil lastRun pointer must still run (and not panic)")
	}
}

func TestRunAutoMergeSweepIfDue_SweepErrorLoggedAndLastRunStamped(t *testing.T) {
	var requests atomic.Int64
	srv := newSweepAPI(t, ``, http.StatusInternalServerError, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var buf strings.Builder
	var lastRun time.Time
	runAutoMergeSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(&buf))

	if !strings.Contains(buf.String(), "automerge sweep failed") {
		t.Fatalf("sweep API failure must be logged as a warning, log:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "automerge sweep complete") {
		t.Fatalf("failed sweep must not log completion, log:\n%s", buf.String())
	}
	if lastRun.IsZero() {
		t.Fatal("lastRun must be stamped before the sweep runs, so a failing sweep still backs off")
	}
}

func TestRunAutoMergeSweepIfDue_LogsCompletionWithSeenAndSkipped(t *testing.T) {
	var requests atomic.Int64
	// One labelled open issue that IS a pull request; its PR fetch 500s, so the
	// sweep records it as seen+skipped and merges nothing.
	issues := `[{"number":7,"pull_request":{"url":"pr-url"}},{"number":8}]`
	srv := newSweepAPI(t, issues, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var buf strings.Builder
	var lastRun time.Time
	runAutoMergeSweepIfDue(context.Background(), ghClient, nil, &lastRun, sweepTestLogger(&buf))

	log := buf.String()
	if !strings.Contains(log, "automerge sweep complete") {
		t.Fatalf("sweep with seen>0 must log completion, log:\n%s", log)
	}
	if !strings.Contains(log, "seen=1") || !strings.Contains(log, "merged=0") || !strings.Contains(log, "skipped=1") {
		t.Fatalf("completion log must report seen=1 merged=0 skipped=1 (issue 8 is not a PR), log:\n%s", log)
	}
}

// resetClaimLedgerForTest resets the package-level sync.Once and pre-installs
// a ledger backed by a temp file, so guard tests never touch the hardwired
// /data ledger path (github.ClaimLedgerPath is a const). Process state is
// restored on cleanup.
func resetClaimLedgerForTest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	oldPath := claimLedgerPath
	oldLoader := claimLedgerLoader
	claimLedgerPath = path
	claimLedgerLoader = github.LoadClaimLedger
	claimLedgerOnce = sync.Once{}
	claimLedgerOnce.Do(func() {
		claimLedger = github.NewClaimLedger(path, sweepTestLogger(nil))
	})
	t.Cleanup(func() {
		claimLedgerOnce = sync.Once{}
		claimLedger = nil
		claimLedgerPath = oldPath
		claimLedgerLoader = oldLoader
	})
	return path
}

func TestGetClaimLedger_LoadsFromInjectedPathAndSurvivesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-claims.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("write corrupt ledger: %v", err)
	}
	oldPath := claimLedgerPath
	oldLoader := claimLedgerLoader
	claimLedgerPath = path
	claimLedgerLoader = github.LoadClaimLedger
	claimLedgerOnce = sync.Once{}
	claimLedger = nil
	t.Cleanup(func() {
		claimLedgerOnce = sync.Once{}
		claimLedger = nil
		claimLedgerPath = oldPath
		claimLedgerLoader = oldLoader
	})

	var buf strings.Builder
	ledger := getClaimLedger(sweepTestLogger(&buf))
	if ledger == nil {
		t.Fatal("corrupt ledger should still return a usable empty ledger")
	}
	if !strings.Contains(buf.String(), path) {
		t.Fatalf("warning should include injected path %q, log: %s", path, buf.String())
	}
}

func TestGetClaimLedger_ReturnsSamePointerOnEveryCall(t *testing.T) {
	resetClaimLedgerForTest(t)
	first := getClaimLedger(sweepTestLogger(nil))
	second := getClaimLedger(sweepTestLogger(nil))
	if first == nil {
		t.Fatal("getClaimLedger must never return nil once the Once has fired")
	}
	if first != second {
		t.Fatal("getClaimLedger must return the same ledger pointer on every call (sync.Once publication)")
	}
}

// guardConfig builds the minimal config applyDuplicatePRGuard consults:
// project identity (whose PRs count as "ours") and escalation disabled so the
// red+stale release valve stays out of these wiring tests.
func guardConfig() *config.Config {
	return &config.Config{
		Project:    config.ProjectConfig{Org: "testorg", AIAuthor: "hive-ai"},
		Escalation: config.EscalationConfig{Disabled: true},
	}
}

func TestApplyDuplicatePRGuard_SuppressesIssueClaimedByHivePR(t *testing.T) {
	ledgerPath := resetClaimLedgerForTest(t)

	// One open PR authored by the hive's ai_author whose title claims issue 12.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/widget/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":9,"state":"open","title":"Fixes #12","body":"","user":{"login":"hive-ai"},"head":{"ref":"quality/fix"},"html_url":"pr-9-url"}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{
			{Repo: "widget", Number: 12, Title: "claimed"},
			{Repo: "widget", Number: 13, Title: "unclaimed"},
		}},
	}
	applyDuplicatePRGuard(context.Background(), guardConfig(), ghClient, actionable, sweepTestLogger(nil))

	if len(actionable.Issues.Items) != 1 || actionable.Issues.Items[0].Number != 13 {
		t.Fatalf("issue 12 (claimed by hive PR 9) must be suppressed and 13 kept, got %+v", actionable.Issues.Items)
	}
	if _, err := os.Stat(ledgerPath); err != nil {
		t.Fatalf("guard must persist the reconciled ledger to %s: %v", ledgerPath, err)
	}
}

func TestApplyDuplicatePRGuard_FetchFailureFailsClosedWithoutSuppressing(t *testing.T) {
	resetClaimLedgerForTest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{
			{Repo: "widget", Number: 12, Title: "kept"},
		}},
	}
	var buf strings.Builder
	applyDuplicatePRGuard(context.Background(), guardConfig(), ghClient, actionable, sweepTestLogger(&buf))

	if len(actionable.Issues.Items) != 1 {
		t.Fatalf("a claim-fetch failure with an empty ledger must suppress nothing, got %+v", actionable.Issues.Items)
	}
	if !strings.Contains(buf.String(), "claim fetch failed") {
		t.Fatalf("claim-fetch failure must be logged, log:\n%s", buf.String())
	}
}
