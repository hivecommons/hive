package dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/modelfamily"
	"github.com/hivecommons/hive/pkg/tokens"
)

func hiveCostUSD(v float64) *float64 { return &v }

func hiveCostBucket(t *testing.T, resp governorPRModelsResponse, rawModel string) governorPRModelBucket {
	t.Helper()
	model := modelfamily.Canonical(rawModel)
	for _, b := range resp.Buckets {
		if b.Model == model {
			return b
		}
	}
	t.Fatalf("no bucket for %q (canonical %q) in %+v", rawModel, model, resp.Buckets)
	return governorPRModelBucket{}
}

func hiveCostApprox(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestGovernorPRModelsHiveCostJoin(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	recent := now.AddDate(0, 0, -2)
	prs := []ghpkg.PullRequest{
		// opus: a short configured repo name with a full URL, a full repo
		// name with no URL, an open PR with no ref, and one PR outside 30d.
		{Repo: "repo", Number: 1, URL: "https://github.com/org/repo/pull/1", HiveAttributed: true, HiveModel: "opus", HiveBackend: "claude", CreatedAt: recent, MergedAt: now},
		{Repo: "org/repo", Number: 2, HiveAttributed: true, HiveModel: "opus", HiveBackend: "claude", CreatedAt: recent, MergedAt: now},
		{Repo: "org/repo", Number: 3, HiveAttributed: true, HiveModel: "opus", HiveBackend: "claude", CreatedAt: recent, State: "open"},
		{Repo: "org/repo", Number: 9, HiveAttributed: true, HiveModel: "opus", HiveBackend: "claude", CreatedAt: now.AddDate(0, 0, -40), MergedAt: now},
		// sonnet: its ref was recorded with a short repo name.
		{Repo: "org/other", Number: 5, HiveAttributed: true, HiveModel: "sonnet", HiveBackend: "copilot", CreatedAt: recent, MergedAt: now},
		// gpt-6.1-sol: a ref exists but carried no usage → usd null.
		{Repo: "org/repo", Number: 8, HiveAttributed: true, HiveModel: "gpt-6.1-sol", HiveBackend: "codex", CreatedAt: recent, MergedAt: now},
		// haiku: no refs at all.
		{Repo: "org/repo", Number: 11, HiveAttributed: true, HiveModel: "haiku", HiveBackend: "claude", CreatedAt: recent, MergedAt: now},
	}
	snap := collect.LaunchCostResponse{
		Ready:       true,
		WindowHours: 336,
		Disclaimer:  collect.CostEstimateDisclaimer,
		ByRef: []collect.RefCostEntry{
			{Ref: "Org/Repo#1", Kind: tokens.LaunchRefKindPR, Repo: "Org/Repo", Number: 1, Launches: 2, ShareUSD: hiveCostUSD(1.5), LaunchUSD: hiveCostUSD(9)},
			{Ref: "org/repo#2", Kind: tokens.LaunchRefKindPR, Repo: "org/repo", Number: 2, Launches: 1, ShareUSD: hiveCostUSD(0.5), LaunchUSD: hiveCostUSD(0.5)},
			{Ref: "org/repo#3", Kind: tokens.LaunchRefKindIssue, Repo: "org/repo", Number: 3, Launches: 4, ShareUSD: hiveCostUSD(4)},
			{Ref: "org/repo#9", Kind: tokens.LaunchRefKindPR, Repo: "org/repo", Number: 9, Launches: 5, ShareUSD: hiveCostUSD(10)},
			{Ref: "other#5", Kind: tokens.LaunchRefKindPR, Repo: "other", Number: 5, Launches: 3, ShareUSD: hiveCostUSD(2)},
			{Ref: "org/repo#8", Kind: tokens.LaunchRefKindPR, Repo: "org/repo", Number: 8, Launches: 1},
		},
		Unattributed:       collect.RepoCostEntry{Repo: collect.RepoCostBucketUnattributed, USD: hiveCostUSD(7.25)},
		BackendUnsupported: collect.RepoCostEntry{Repo: collect.RepoCostBucketBackendUnsupported},
	}

	resp := aggregateGovernorPRModels(prs, governorPRModelsWindow30d, now)
	applyGovernorPRModelHiveCost(&resp, prs, snap, true, now)

	cov := resp.HiveCostCoverage
	if !cov.Ready || cov.WindowHours != 336 || cov.Disclaimer != collect.CostEstimateDisclaimer {
		t.Fatalf("hive_cost_coverage = %+v", cov)
	}
	if cov.UnattributedUSD == nil || *cov.UnattributedUSD != 7.25 || cov.BackendUnsupportedUSD != nil {
		t.Fatalf("coverage buckets = unattributed %v backend_unsupported %v", cov.UnattributedUSD, cov.BackendUnsupportedUSD)
	}

	tests := []struct {
		model       string
		usd         *float64
		launches    int
		prsWithCost int
		perMerged   *float64
	}{
		// #1 (1.5, short repo + URL) + #2 (0.5); #3 is an issue ref, #9 is outside the window.
		{model: "opus", usd: hiveCostUSD(2), launches: 3, prsWithCost: 2, perMerged: hiveCostUSD(1)},
		{model: "sonnet", usd: hiveCostUSD(2), launches: 3, prsWithCost: 1, perMerged: hiveCostUSD(2)},
		{model: "gpt-6.1-sol", usd: nil, launches: 1, prsWithCost: 1, perMerged: nil},
		{model: "haiku", usd: nil, launches: 0, prsWithCost: 0, perMerged: nil},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			hc := hiveCostBucket(t, resp, tc.model).HiveCost
			if hc == nil {
				t.Fatalf("hive_cost missing")
			}
			if (hc.USD == nil) != (tc.usd == nil) || (hc.USD != nil && !hiveCostApprox(*hc.USD, *tc.usd)) {
				t.Fatalf("usd = %v, want %v", hc.USD, tc.usd)
			}
			if (hc.USDPerMergedPR == nil) != (tc.perMerged == nil) || (hc.USDPerMergedPR != nil && !hiveCostApprox(*hc.USDPerMergedPR, *tc.perMerged)) {
				t.Fatalf("usd_per_merged_pr = %v, want %v", hc.USDPerMergedPR, tc.perMerged)
			}
			if hc.Launches != tc.launches || hc.PRsWithCost != tc.prsWithCost {
				t.Fatalf("launches/prs_with_cost = %d/%d, want %d/%d", hc.Launches, hc.PRsWithCost, tc.launches, tc.prsWithCost)
			}
		})
	}

	// Null is not zero on the wire.
	raw, err := json.Marshal(hiveCostBucket(t, resp, "haiku"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hive_cost":{"usd":null,`) {
		t.Fatalf("haiku bucket JSON = %s, want usd null", raw)
	}

	// The all window counts #9 too.
	all := aggregateGovernorPRModels(prs, governorPRModelsWindowAll, now)
	applyGovernorPRModelHiveCost(&all, prs, snap, true, now)
	if hc := hiveCostBucket(t, all, "opus").HiveCost; hc == nil || hc.USD == nil || !hiveCostApprox(*hc.USD, 12) || hc.PRsWithCost != 3 {
		t.Fatalf("all-window opus hive_cost = %+v", hc)
	}
}

func TestGovernorPRModelsHiveCostShortPRRepo(t *testing.T) {
	now := time.Now().UTC()
	prs := []ghpkg.PullRequest{
		{Repo: "repo", Number: 4, HiveAttributed: true, HiveModel: "opus", CreatedAt: now, MergedAt: now},
		{Repo: "dup", Number: 6, HiveAttributed: true, HiveModel: "sonnet", CreatedAt: now, MergedAt: now},
	}
	snap := collect.LaunchCostResponse{Ready: true, ByRef: []collect.RefCostEntry{
		{Ref: "org/repo#4", Kind: tokens.LaunchRefKindPR, Repo: "org/repo", Number: 4, Launches: 1, ShareUSD: hiveCostUSD(3)},
		// Two owners have dup#6: ambiguous, so neither is joined.
		{Ref: "a/dup#6", Kind: tokens.LaunchRefKindPR, Repo: "a/dup", Number: 6, Launches: 1, ShareUSD: hiveCostUSD(1)},
		{Ref: "b/dup#6", Kind: tokens.LaunchRefKindPR, Repo: "b/dup", Number: 6, Launches: 1, ShareUSD: hiveCostUSD(1)},
	}}
	resp := aggregateGovernorPRModels(prs, governorPRModelsWindowAll, now)
	applyGovernorPRModelHiveCost(&resp, prs, snap, true, now)
	if hc := hiveCostBucket(t, resp, "opus").HiveCost; hc == nil || hc.USD == nil || *hc.USD != 3 || hc.PRsWithCost != 1 {
		t.Fatalf("short PR repo vs full ref: hive_cost = %+v", hc)
	}
	if hc := hiveCostBucket(t, resp, "sonnet").HiveCost; hc == nil || hc.USD != nil || hc.PRsWithCost != 0 {
		t.Fatalf("ambiguous owner: hive_cost = %+v, want no match", hc)
	}
}

func TestGovernorPRModelsHiveCostNotReady(t *testing.T) {
	now := time.Now().UTC()
	prs := []ghpkg.PullRequest{{Repo: "org/repo", Number: 1, HiveAttributed: true, HiveModel: "opus", CreatedAt: now, MergedAt: now}}
	resp := aggregateGovernorPRModels(prs, governorPRModelsWindow7d, now)
	snap := collect.LaunchCostResponse{ByRef: []collect.RefCostEntry{{Ref: "org/repo#1", Kind: tokens.LaunchRefKindPR, Repo: "org/repo", Number: 1, Launches: 1, ShareUSD: hiveCostUSD(1)}}}
	applyGovernorPRModelHiveCost(&resp, prs, snap, false, now)
	if resp.HiveCostCoverage.Ready || resp.HiveCostCoverage.Disclaimer == "" {
		t.Fatalf("coverage = %+v, want ready=false with disclaimer", resp.HiveCostCoverage)
	}
	for _, b := range resp.Buckets {
		if b.HiveCost != nil {
			t.Fatalf("bucket %s has hive_cost %+v while snapshot not ready", b.Model, b.HiveCost)
		}
	}

	// Through the handler: no collector, then a collector that has not
	// collected yet. Both answer 200 without hive_cost on any bucket.
	for _, name := range []string{"no collector", "not ready"} {
		t.Run(name, func(t *testing.T) {
			s := covApiServer(t)
			s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{PRs: ghpkg.PRResult{Attributed: prs}}}
			if name == "not ready" {
				s.deps.LaunchCost = collect.NewLaunchCostCollector(&fakeLaunchTokens{summary: &tokens.AggregateSummary{}}, nil)
			}
			rec := httptest.NewRecorder()
			s.handleGovernorPRModels(rec, httptest.NewRequest(http.MethodGet, "/api/governor/pr-models?window=7d", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if strings.Contains(body, `"hive_cost":`) {
				t.Fatalf("hive_cost present while not ready: %s", body)
			}
			var got governorPRModelsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.HiveCostCoverage.Ready || len(got.Buckets) != 1 || got.Buckets[0].PRs != 1 {
				t.Fatalf("response = %+v", got)
			}
		})
	}
}
