package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/modelfamily"
)

func closedReasonsFor(t *testing.T, resp governorPRModelsResponse, model string) governorPRModelBucket {
	t.Helper()
	for _, b := range resp.Buckets {
		if b.Model == modelfamily.Canonical(model) {
			return b
		}
	}
	t.Fatalf("no bucket %q in %+v", model, resp.Buckets)
	return governorPRModelBucket{}
}

func assertCloseReasonsSum(t *testing.T, resp governorPRModelsResponse) {
	t.Helper()
	for _, b := range resp.Buckets {
		r := b.ClosedUnmergedReasons
		if r.Superseded+r.Duplicate+r.Other != b.ClosedUnmerged {
			t.Fatalf("bucket %s: reasons %+v do not sum to closed_unmerged %d", b.Model, r, b.ClosedUnmerged)
		}
	}
}

func TestGovernorPRModelsCloseReasons(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	closed := func(number int, model string, labels ...string) ghpkg.PullRequest {
		return ghpkg.PullRequest{
			Repo: "org/repo", Number: number, URL: "https://github.com/org/repo/pull/" + strconv.Itoa(number),
			HiveAttributed: true, HiveModel: model, HiveBackend: "claude",
			CreatedAt: now.AddDate(0, 0, -1), State: "closed", ClosedAt: now, Labels: labels,
		}
	}
	attributed := []ghpkg.PullRequest{
		closed(1, "opus", ghpkg.SupersededLabel),          // superseded by label
		closed(2, "opus"),                                 // superseded by another PR (#10 below)
		closed(3, "opus", "Duplicate"),                    // duplicate (case-insensitive)
		closed(4, "opus"),                                 // other
		closed(5, "opus", "duplicate", "hive/superseded"), // both: superseded wins
		closed(6, "sonnet"),                               // other; #20 in another repo supersedes a #6 there
		{Repo: "org/repo", Number: 10, URL: "https://github.com/org/repo/pull/10", HiveAttributed: true, HiveModel: "opus", CreatedAt: now.AddDate(0, 0, -1), MergedAt: now, Supersedes: 2},
		{Repo: "org/repo", Number: 11, URL: "https://github.com/org/repo/pull/11", HiveAttributed: true, HiveModel: "opus", CreatedAt: now.AddDate(0, 0, -1), State: "open"},
		// Outside the 7d window: not counted anywhere.
		{Repo: "org/repo", Number: 12, HiveAttributed: true, HiveModel: "opus", CreatedAt: now.AddDate(0, 0, -20), State: "closed", ClosedAt: now, Labels: []string{"duplicate"}},
	}
	open := []ghpkg.PullRequest{
		{Repo: "org/other", Number: 20, URL: "https://github.com/org/other/pull/20", State: "open", Supersedes: 6},
	}
	superseded := governorPRModelsSupersededKeys(attributed, open)

	resp := aggregateGovernorPRModels(attributed, "7d", now)
	applyGovernorPRModelCloseReasons(&resp, attributed, superseded, now)
	assertCloseReasonsSum(t, resp)

	opus := closedReasonsFor(t, resp, "opus")
	if opus.ClosedUnmerged != 5 || opus.Merged != 1 || opus.Open != 1 {
		t.Fatalf("opus outcomes changed: %+v", opus)
	}
	if want := (governorPRModelCloseReasons{Superseded: 3, Duplicate: 1, Other: 1}); opus.ClosedUnmergedReasons != want {
		t.Fatalf("opus reasons = %+v, want %+v", opus.ClosedUnmergedReasons, want)
	}
	sonnet := closedReasonsFor(t, resp, "sonnet")
	if want := (governorPRModelCloseReasons{Other: 1}); sonnet.ClosedUnmergedReasons != want {
		t.Fatalf("sonnet reasons = %+v, want %+v (a Supersedes in another repo must not match)", sonnet.ClosedUnmergedReasons, want)
	}

	all := aggregateGovernorPRModels(attributed, "all", now)
	applyGovernorPRModelCloseReasons(&all, attributed, superseded, now)
	assertCloseReasonsSum(t, all)
	if got := closedReasonsFor(t, all, "opus").ClosedUnmergedReasons.Duplicate; got != 2 {
		t.Fatalf("all-window opus duplicate = %d, want 2", got)
	}
}

func TestHandleGovernorPRModelsCloseReasonsFromOpenSuperseder(t *testing.T) {
	s := covApiServer(t)
	now := time.Now().UTC()
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{
		PRs: ghpkg.PRResult{
			Attributed: []ghpkg.PullRequest{
				{Repo: "org/repo", Number: 1, URL: "https://github.com/org/repo/pull/1", HiveAttributed: true, HiveModel: "opus", CreatedAt: now, State: "closed", ClosedAt: now},
				{Repo: "org/repo", Number: 2, URL: "https://github.com/org/repo/pull/2", HiveAttributed: true, HiveModel: "opus", CreatedAt: now, State: "closed", ClosedAt: now},
			},
			Items: []ghpkg.PullRequest{
				{Repo: "org/repo", Number: 3, URL: "https://github.com/org/repo/pull/3", State: "open", Supersedes: 1},
			},
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/governor/pr-models?window=7d", nil)
	rec := httptest.NewRecorder()
	s.handleGovernorPRModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var raw struct {
		Buckets []map[string]json.RawMessage `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(raw.Buckets))
	}
	var reasons governorPRModelCloseReasons
	if err := json.Unmarshal(raw.Buckets[0]["closed_unmerged_reasons"], &reasons); err != nil {
		t.Fatalf("closed_unmerged_reasons missing or malformed: %v", err)
	}
	if want := (governorPRModelCloseReasons{Superseded: 1, Other: 1}); reasons != want {
		t.Fatalf("closed_unmerged_reasons = %+v, want %+v", reasons, want)
	}
	if string(raw.Buckets[0]["closed_unmerged"]) != "2" {
		t.Fatalf("closed_unmerged = %s, want 2 (unchanged)", raw.Buckets[0]["closed_unmerged"])
	}
}
