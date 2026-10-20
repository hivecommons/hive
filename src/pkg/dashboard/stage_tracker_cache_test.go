package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// TestStageTrackerListsSidelinedItems pins the completeness fix of
// hivecommons/hive#12682: the issues the enumeration parks in a work-detail
// bucket (claimed, covered by a PR, advisory, filtered) and the PRs it
// sidelines (drafts, exempt labels) are rows too, with the bucket named,
// the labels carried, and a stage derived from the bucket when neither a
// stage label nor a store record exists. Nothing is listed twice. Standing
// meta issues (hive_advisory, dependency_dashboard) are the exception: they
// are not pipeline work and stay off the tracker (hivecommons/hive#13257).
func TestStageTrackerListsSidelinedItems(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	created := now.Add(-3 * 24 * time.Hour)

	s.statusMu.Lock()
	repo := &s.status.Repos[0]
	repo.NonActionableIssues = []any{
		github.RepoWorkIssue{Repo: "acme/console", Number: 9, Title: "Covered, unlabelled", Author: "mina", Labels: []string{"kind/bug"}, Bucket: "claimed_by_pr", Reason: "Open external PR #103 covers this issue", CreatedAt: created,
			LinkedPRs: []github.IssueLinkedPR{{Number: 103, State: "open"}}},
		&github.RepoWorkIssue{Repo: "acme/console", Number: 10, Title: "Advisory", Author: "hive-app[bot]", Labels: []string{"hive-advisory"}, Bucket: "hive_advisory", Reason: "Hive advisory report", CreatedAt: created},
		github.RepoWorkIssue{Repo: "acme/console", Number: 11, Title: "Parked on a person", Author: "mina", Labels: []string{"needs-human", "stage/085-needs-decision"}, Bucket: "filtered", Reason: "needs-human", CreatedAt: created},
		github.RepoWorkIssue{Repo: "acme/console", Number: 12, Title: "Live claim", Author: "mina", Bucket: "claimed", Reason: "Live issue claim by alice", CreatedAt: created},
		// Already an actionable row: must not be duplicated.
		github.RepoWorkIssue{Repo: "acme/console", Number: 8, Title: "Flaky test (dup)", Bucket: "filtered"},
	}
	repo.NonActionablePrs = []any{
		github.RepoWorkPR{Repo: "acme/console", Number: 106, Title: "WIP draft", Author: "zoe", Labels: []string{"author/clanker"}, Draft: true, HiveAttributed: true, Bucket: "draft", Reason: "Draft", CreatedAt: created},
		&github.RepoWorkPR{Repo: "acme/console", Number: 107, Title: "Exempt", Author: "pat", Labels: []string{"do-not-merge"}, Bucket: "filtered", Reason: "Exempt label", CreatedAt: created},
	}
	s.statusMu.Unlock()

	resp := s.buildStageTracker(s.status, store, now)
	if len(resp.Items) != 13 {
		t.Fatalf("items = %d, want 13 (8 fixture + 3 parked issues + 2 sidelined PRs; the advisory stays off)", len(resp.Items))
	}
	seen := map[string]int{}
	for _, it := range resp.Items {
		seen[it.Kind+"#"+strconv.Itoa(it.Number)]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("%s listed %d times", k, n)
		}
	}
	issue9 := trackerItem(t, resp, "issue", 9)
	if issue9.Stage != trackerNumberCoveredByPR || issue9.Bucket != "claimed_by_pr" || issue9.BucketReason == "" || len(issue9.Labels) != 1 || issue9.CreatedAt == nil || !issue9.CreatedAt.Equal(created) {
		t.Fatalf("issue9 = %+v", issue9)
	}
	if len(issue9.LinkedPRs) != 1 || issue9.LinkedPRs[0].Number != 103 || issue9.LinkedPRs[0].Title != "Relay PR" {
		t.Fatalf("issue9 linked PRs = %+v", issue9.LinkedPRs)
	}
	if _, listed := seen["issue#10"]; listed {
		t.Fatalf("standing hive_advisory issue10 must stay off the tracker (#13257)")
	}
	issue11 := trackerItem(t, resp, "issue", 11)
	// A pre-045 issue still wearing 085 keeps that stage until the reconciler
	// re-stages it; the tracker flags the mismatch as mirrored.
	if issue11.Stage != trackerNumberNeedsDecision || !issue11.Mirrored || !hasMod(issue11, modNeedsHuman) {
		t.Fatalf("label beats bucket: issue11 = %+v", issue11)
	}
	if issue12 := trackerItem(t, resp, "issue", 12); issue12.Stage != trackerNumberClaimed {
		t.Fatalf("issue12 stage = %d, want claimed", issue12.Stage)
	}
	if issue8 := trackerItem(t, resp, "issue", 8); issue8.Title != "Flaky test" || issue8.Bucket != "" {
		t.Fatalf("actionable row wins over its work-detail echo: %+v", issue8)
	}
	pr106 := trackerItem(t, resp, "pr", 106)
	if !hasMod(pr106, modDraft) || pr106.Bucket != "draft" || pr106.AuthorKind != authorKindClanker || pr106.AuthorKindSource != authorKindSrcLabel || pr106.Lane != laneContributor {
		t.Fatalf("draft pr106 = %+v", pr106)
	}
	if pr107 := trackerItem(t, resp, "pr", 107); pr107.Bucket != "filtered" || pr107.BucketReason != "Exempt label" || pr107.Stage != 0 {
		t.Fatalf("exempt pr107 = %+v", pr107)
	}
}

// TestStageTrackerCacheServesWithoutGitHub proves the cache contract:
// the first request builds (source=fresh), later requests inside the TTL
// are byte-identical cache hits with the same ETag, If-None-Match answers
// 304, the TTL knob rebuilds once it lapses, and a GitHub client that
// counts every request sees zero throughout. The REST accounting lists the
// endpoint with Charged 0.
func TestStageTrackerCacheServesWithoutGitHub(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	s, _ := stageTrackerFixture(t, now)
	deps := testDeps(t)
	deps.Config.Dashboard.StageTracker.CacheTTLS = 20
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("stage tracker reached GitHub: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(gh.Close)
	deps.GHClient = github.NewClientForTest(gh.URL, "acme", []string{"console"}, covBLogger())
	s.deps = deps

	get := func(ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		rr := httptest.NewRecorder()
		s.handleStageTracker(rr, req)
		return rr
	}
	before := stageTrackerRESTAccounting()
	first := get("")
	if first.Code != http.StatusOK || first.Header().Get("ETag") == "" || first.Header().Get("X-Hive-Stage-Tracker-Source") != "fresh" {
		t.Fatalf("first: %d etag=%q src=%q", first.Code, first.Header().Get("ETag"), first.Header().Get("X-Hive-Stage-Tracker-Source"))
	}
	if cc := first.Header().Get("Cache-Control"); cc != "private, max-age=20" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	var fresh StageTrackerResponse
	if err := json.Unmarshal(first.Body.Bytes(), &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Source != "fresh" || fresh.CacheTTLSeconds != 20 || fresh.GeneratedAt.IsZero() || len(fresh.Items) != 8 {
		t.Fatalf("fresh payload: source=%q ttl=%d at=%v items=%d", fresh.Source, fresh.CacheTTLSeconds, fresh.GeneratedAt, len(fresh.Items))
	}
	etag := first.Header().Get("ETag")

	second := get("")
	if second.Code != http.StatusOK || second.Header().Get("ETag") != etag || second.Header().Get("X-Hive-Stage-Tracker-Source") != "cache" {
		t.Fatalf("second: %d etag=%q src=%q", second.Code, second.Header().Get("ETag"), second.Header().Get("X-Hive-Stage-Tracker-Source"))
	}
	var cached StageTrackerResponse
	if err := json.Unmarshal(second.Body.Bytes(), &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Source != "cache" || !cached.GeneratedAt.Equal(fresh.GeneratedAt) || len(cached.Items) != len(fresh.Items) {
		t.Fatalf("cached payload: source=%q at=%v (fresh %v)", cached.Source, cached.GeneratedAt, fresh.GeneratedAt)
	}
	if ma := second.Header().Get("Cache-Control"); !strings.HasPrefix(ma, "private, max-age=") {
		t.Fatalf("second Cache-Control = %q", ma)
	}
	if third := get(etag); third.Code != http.StatusNotModified || third.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d body=%d", third.Code, third.Body.Len())
	}
	if miss := get(`"nope"`); miss.Code != http.StatusOK {
		t.Fatalf("stale validator: %d", miss.Code)
	}

	// Past the TTL the next request rebuilds.
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerCache.generatedAt = s.stageTrackerCache.generatedAt.Add(-21 * time.Second)
	s.stageTrackerCacheMu.Unlock()
	if again := get(etag); again.Code != http.StatusOK || again.Header().Get("X-Hive-Stage-Tracker-Source") != "fresh" {
		t.Fatalf("after TTL: %d src=%q", again.Code, again.Header().Get("X-Hive-Stage-Tracker-Source"))
	}

	// The REST accounting store is process-wide, so other tests in the
	// package also drive this caller; only the delta from this test counts.
	after := stageTrackerRESTAccounting()
	if after.Requests-before.Requests != 5 || after.Charged != before.Charged || after.NotModified-before.NotModified != 1 {
		t.Fatalf("accounting delta: before=%+v after=%+v", before, after)
	}
}

// stageTrackerRESTAccounting sums the REST accounting rows recorded under the
// stage tracker's caller across endpoints, without a limit that could drop it
// behind busier consumers.
func stageTrackerRESTAccounting() github.RESTConsumer {
	var sum github.RESTConsumer
	sum.Caller = stageTrackerRESTCaller
	for _, c := range github.RESTTopConsumers(1 << 20) {
		if c.Caller == stageTrackerRESTCaller {
			sum.Requests += c.Requests
			sum.Charged += c.Charged
			sum.NotModified += c.NotModified
		}
	}
	return sum
}

func TestStageTrackerCacheTTLDefaultsToEvalInterval(t *testing.T) {
	s := newTestServer()
	deps := testDeps(t)
	deps.Config.Governor.EvalIntervalS = 123
	s.deps = deps
	if got := s.stageTrackerCacheTTL(); got != 123*time.Second {
		t.Fatalf("default ttl = %v, want eval interval", got)
	}
	deps.Config.Dashboard.StageTracker.CacheTTLS = 7
	if got := s.stageTrackerCacheTTL(); got != 7*time.Second {
		t.Fatalf("knob ttl = %v", got)
	}
	s.deps = nil
	if got := s.stageTrackerCacheTTL(); got != 300*time.Second {
		t.Fatalf("no-config ttl = %v", got)
	}
}
