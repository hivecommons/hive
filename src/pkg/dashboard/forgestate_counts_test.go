package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// fakeForgeOpen scripts the cache's open totals per repo for buildRepos.
type fakeForgeOpen map[string]forgestate.OpenCounts

func (f fakeForgeOpen) Open(repo string) (forgestate.OpenCounts, bool) {
	c, ok := f[strings.ToLower(repo)]
	return c, ok
}

func forgeCountsCfg() *config.Config {
	return &config.Config{Project: config.ProjectConfig{Org: "o", Repos: []string{"a", "x/b", "c"}}}
}

func TestBuildReposOverviewCountsComeFromForgeCache(t *testing.T) {
	cfg := forgeCountsCfg()
	actionable := &github.ActionableResult{
		// The enumeration's own listing: refused for o/a (no totals), and
		// x/b classified 2 of what the cache says are 5 open issues.
		TotalByRepo: map[string]github.RepoCounts{"x/b": {Issues: 2, PRs: 1}, "c": {Issues: 7, PRs: 3}},
		WorkBreakdownByRepo: map[string]github.RepoWorkBreakdown{
			"x/b": {Issues: github.RepoIssueBreakdown{Actionable: 1, Hold: 1}, PRs: github.RepoPRBreakdown{Actionable: 1}},
		},
	}
	staleAt := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	forge := fakeForgeOpen{
		"o/a": {Issues: 4, PRs: 2, Ready: true},
		"x/b": {Issues: 5, PRs: 1, Ready: true, Stale: true, StaleSince: staleAt},
		"o/c": {Ready: false},
	}
	repos := buildReposWithForge(cfg, actionable, governor.State{}, forge)
	byName := map[string]FrontendRepo{}
	for _, r := range repos {
		byName[r.Name] = r
	}

	// o/a: the enumeration refused the repo; the cache fills the totals and
	// clears the incomplete mark so the Overview never reads zero.
	if a := byName["a"]; a.Issues != 4 || a.PRs != 2 || a.CountsIncomplete || a.CountsStale {
		t.Fatalf("a = issues %d prs %d incomplete %v stale %v, want 4/2 from cache", a.Issues, a.PRs, a.CountsIncomplete, a.CountsStale)
	}
	// x/b: cache totals win; the 3 issues the enumeration did not classify
	// land in Other so the partition still sums to the total; stale carries
	// its since mark.
	b := byName["x/b"]
	if b.Issues != 5 || b.PRs != 1 {
		t.Fatalf("x/b = %d/%d, want cache 5/1", b.Issues, b.PRs)
	}
	if b.WorkBreakdown == nil || b.WorkBreakdown.Issues.Total() != 5 || b.WorkBreakdown.Issues.Other != 3 || b.WorkBreakdown.Issues.Actionable != 1 {
		t.Fatalf("x/b breakdown = %+v, want total 5 with other 3", b.WorkBreakdown)
	}
	if b.WorkBreakdown.PRs.Total() != 1 || b.WorkBreakdown.PRs.Other != 0 {
		t.Fatalf("x/b PR breakdown = %+v, want unchanged total 1", b.WorkBreakdown.PRs)
	}
	if !b.CountsStale || b.CountsStaleSince != "2026-10-21T12:00:00Z" {
		t.Fatalf("x/b stale = %v since %q", b.CountsStale, b.CountsStaleSince)
	}
	// c: cache not ready → the enumeration's numbers stand.
	if c := byName["c"]; c.Issues != 7 || c.PRs != 3 || c.CountsStale {
		t.Fatalf("c = %d/%d stale %v, want enumeration 7/3", c.Issues, c.PRs, c.CountsStale)
	}

	// The 3-arg wrapper is the no-cache path.
	plain := buildRepos(cfg, actionable, governor.State{})
	for _, r := range plain {
		if r.Name == "a" && (!r.CountsIncomplete || r.Issues != 0) {
			t.Fatalf("without a cache a refused repo stays incomplete: %+v", r)
		}
	}

	if got := overviewStaleSince(repos); got != "2026-10-21T12:00:00Z" {
		t.Fatalf("overviewStaleSince = %q", got)
	}
	if got := overviewStaleSince(plain); got != "" {
		t.Fatalf("overviewStaleSince without stale repos = %q", got)
	}
	status := &StatusPayload{Repos: repos}
	if overviewKPISnapshotComplete(status) {
		t.Fatal("a stale repo must keep the snapshot out of KPI history")
	}

	// The overview totals read the overlaid numbers.
	totals := overviewTotals(status)
	if totals.Issues.Forge != 4+5+7 || totals.PRs.Forge != 2+1+3 {
		t.Fatalf("overview forge totals = %d/%d, want 16/6", totals.Issues.Forge, totals.PRs.Forge)
	}
}

func TestApplyForgeOpenCountsNeverLowersClassifiedBuckets(t *testing.T) {
	r := FrontendRepo{Issues: 9, PRs: 4, WorkBreakdown: &github.RepoWorkBreakdown{
		Issues: github.RepoIssueBreakdown{Actionable: 3, Hold: 2, Other: 4},
		PRs:    github.RepoPRBreakdown{Actionable: 2, Draft: 1, Other: 1},
	}}
	orig := r.WorkBreakdown
	if !applyForgeOpenCounts(&r, forgestate.OpenCounts{Issues: 5, PRs: 3, Ready: true}) {
		t.Fatal("ready counts must apply")
	}
	if r.Issues != 5 || r.PRs != 3 {
		t.Fatalf("totals = %d/%d", r.Issues, r.PRs)
	}
	if r.WorkBreakdown.Issues.Other != 0 || r.WorkBreakdown.Issues.Actionable != 3 || r.WorkBreakdown.Issues.Hold != 2 {
		t.Fatalf("issue breakdown = %+v, want other shrunk to 0 and classified kept", r.WorkBreakdown.Issues)
	}
	if r.WorkBreakdown.PRs.Other != 0 || r.WorkBreakdown.PRs.Total() != 3 {
		t.Fatalf("pr breakdown = %+v", r.WorkBreakdown.PRs)
	}
	if orig.Issues.Other != 4 {
		t.Fatal("the enumeration's breakdown must not be mutated in place")
	}
	if applyForgeOpenCounts(&r, forgestate.OpenCounts{Issues: 0, Ready: false}) {
		t.Fatal("not-ready counts must not apply")
	}
	if r.Issues != 5 {
		t.Fatal("not-ready counts must leave the repo alone")
	}
}

// fakeOutcomeReconcile stands in for the hourly Search-API check.
type fakeOutcomeReconcile struct {
	mu     sync.Mutex
	merged int
	closed int
	err    error
	calls  int
}

func (f *fakeOutcomeReconcile) fn(_ context.Context, _ string) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.merged, f.closed, f.err
}

func (f *fakeOutcomeReconcile) set(merged, closed int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged, f.closed, f.err = merged, closed, err
}

func newForgeCountsCollector(t *testing.T, now func() time.Time, api *fakeOutcomeReconcile) (*MetricsCollector, *forgestate.Counts) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMetricsCollector(nil, "o", "r", "", "hive-bot[bot]", "P", logger)
	mc.prIssueCachePath = filepath.Join(t.TempDir(), "counts.json")
	mc.SetProjectScope("o", "r", []string{"r"}, "hive-bot[bot]")
	fc := forgestate.NewCounts(forgestate.CountsOptions{
		Repos:     mc.countReposQualified(),
		Reconcile: api.fn,
		Author:    "hive-bot[bot]",
		Now:       now,
	})
	mc.prIssueMu.Lock()
	mc.forgeCounts = fc
	mc.prIssueMu.Unlock()
	return mc, fc
}

func TestCollectPRIssueCountsFromForgeReconcilesHourlyAndCountsEvents(t *testing.T) {
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	api := &fakeOutcomeReconcile{merged: 10, closed: 20}
	mc, fc := newForgeCountsCollector(t, now, api)
	ctx := context.Background()

	if mc.ForgeCounts() != fc {
		t.Fatal("ForgeCounts must expose the counters")
	}
	mc.collectPRIssueCounts(ctx)
	got := mc.GetPRIssueCounts()
	if got == nil || got.MergedPRs != 10 || got.ClosedIssues != 20 || got.Stale || got.Source != "forgestate" || got.Status != "fresh" {
		t.Fatalf("first collect = %+v, want 10/20 fresh from forgestate", got)
	}
	if api.calls != 1 {
		t.Fatalf("search calls = %d, want 1", api.calls)
	}

	// Webhook events move the counters; collects between reconciles do not
	// touch the Search API.
	if moved, err := fc.NoteWebhook("pull_request", []byte(`{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":1,"merged":true,"user":{"login":"hive-bot[bot]"}}}`)); err != nil || !moved {
		t.Fatalf("NoteWebhook = %v, %v", moved, err)
	}
	for i := 0; i < 5; i++ {
		clock = clock.Add(5 * time.Minute)
		mc.collectPRIssueCounts(ctx)
	}
	if got := mc.GetPRIssueCounts(); got.MergedPRs != 11 || got.ClosedIssues != 20 {
		t.Fatalf("after event = %+v, want 11/20", got)
	}
	if api.calls != 1 {
		t.Fatalf("search calls after 25 minutes of collects = %d, want still 1", api.calls)
	}

	// The hourly reconcile overwrites with GitHub's answer.
	api.set(13, 21, nil)
	clock = clock.Add(forgestate.CountsReconcileInterval)
	mc.collectPRIssueCounts(ctx)
	if got := mc.GetPRIssueCounts(); got.MergedPRs != 13 || got.ClosedIssues != 21 || got.Stale {
		t.Fatalf("after reconcile = %+v, want 13/21", got)
	}
	if api.calls != 2 {
		t.Fatalf("search calls = %d, want 2", api.calls)
	}

	// A 403 on the next reconcile keeps last-good + events and sets stale_since.
	fc.NoteClosed("o/r", forgestate.KindIssue, false, "hive-bot[bot]")
	api.set(0, 0, errors.New("403 secondary rate limit"))
	clock = clock.Add(forgestate.CountsReconcileInterval)
	mc.collectPRIssueCounts(ctx)
	got = mc.GetPRIssueCounts()
	if got.MergedPRs != 13 || got.ClosedIssues != 22 {
		t.Fatalf("after 403 = %+v, want last good plus event 13/22, never zero", got)
	}
	if !got.Stale || got.StaleSince != clock.Format(time.RFC3339) || !strings.Contains(got.Status, "rate-limited") {
		t.Fatalf("after 403 = %+v, want stale since %s", got, clock.Format(time.RFC3339))
	}
	// The persisted copy carries the marker too.
	raw, err := os.ReadFile(mc.prIssueCachePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted github.PRIssueCounts
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.StaleSince == "" || persisted.MergedPRs != 13 {
		t.Fatalf("persisted = %+v", persisted)
	}
}

func TestCollectPRIssueCountsFromForgeNotReadyKeepsDiskCounts(t *testing.T) {
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	api := &fakeOutcomeReconcile{err: errors.New("403 rate limit")}
	mc, _ := newForgeCountsCollector(t, func() time.Time { return clock }, api)
	mc.prIssueMu.Lock()
	mc.prIssueCounts = &github.PRIssueCounts{MergedPRs: 7, ClosedIssues: 9, Status: "fresh"}
	mc.prIssueMu.Unlock()

	mc.collectPRIssueCounts(context.Background())
	got := mc.GetPRIssueCounts()
	if got.MergedPRs != 7 || got.ClosedIssues != 9 || !got.Stale || got.StaleSince == "" {
		t.Fatalf("first reconcile refused: counts = %+v, want the disk-loaded 7/9 marked stale with stale_since", got)
	}
}

func TestGitHubWebhookFeedsForgeOutcomeCounters(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	s := newGitHubWebhookTestServer(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMetricsCollector(nil, "webhookorg", "webhookrepo", "", "hive-bot[bot]", "P", logger)
	mc.SetProjectScope("webhookorg", "webhookrepo", []string{"webhookrepo"}, "hive-bot[bot]")
	fc := mc.EnableForgeCounts(nil)
	s.deps.MetricsCollector = mc

	post := func(event, body string) map[string]any {
		t.Helper()
		rec := postGitHubWebhook(s, event, signGitHubWebhookForTest(secret, []byte(body)), []byte(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", event, rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post("pull_request", `{"action":"closed","number":5,"pull_request":{"number":5,"merged":true,"user":{"login":"hive-bot[bot]"}},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if resp["outcome_counted"] != true {
		t.Fatalf("merged PR response = %v, want outcome_counted", resp)
	}
	resp = post("pull_request", `{"action":"closed","number":6,"pull_request":{"number":6,"merged":false,"user":{"login":"hive-bot[bot]"}},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if _, ok := resp["outcome_counted"]; ok {
		t.Fatalf("unmerged close response = %v, want no outcome_counted", resp)
	}
	resp = post("issues", `{"action":"closed","issue":{"number":7,"user":{"login":"hive-bot[bot]"}},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if resp["outcome_counted"] != true {
		t.Fatalf("closed issue response = %v, want outcome_counted", resp)
	}
	got := fc.Outcomes()
	if got.MergedPRs != 1 || got.ClosedIssues != 1 || got.Source != forgestate.SourceWebhook {
		t.Fatalf("counters = %+v, want 1 merged / 1 closed from webhooks", got)
	}
}
