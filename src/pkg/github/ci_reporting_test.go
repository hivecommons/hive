package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stageengine"
)

type ciReportingFixture struct {
	client     *gh.Client
	check      *gh.CheckRun
	mode       string
	reads      atomic.Int64
	heads      atomic.Int64
	beforeRuns func()
}

func newCIReportingFixture(t *testing.T) *ciReportingFixture {
	t.Helper()
	f := &ciReportingFixture{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("read-only observer attempted %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.reads.Add(1)
		run := map[string]any{
			"id": 21, "head_sha": "sha", "check_suite_id": 31, "workflow_id": 11,
			"run_number": 9, "run_attempt": 1, "event": "pull_request",
			"status": "completed", "conclusion": "failure",
			"updated_at": "2026-10-31T00:00:00Z",
			"repository": map[string]string{"full_name": "acme/widget"},
			"html_url":   server.URL + "/acme/widget/actions/runs/21",
		}
		switch f.mode {
		case "active", "old-active":
			run["status"], run["conclusion"] = "queued", ""
		case "success":
			run["conclusion"] = "success"
		case "cancelled":
			run["conclusion"] = "cancelled"
		case "unknown-conclusion":
			run["conclusion"] = "unrecognized"
		case "unknown-run-status":
			run["status"] = "unrecognized"
		case "run-head":
			run["head_sha"] = "other-head"
		case "run-repo":
			run["repository"] = map[string]string{"full_name": "other/widget"}
		case "run-suite":
			run["check_suite_id"] = 99
		case "attempt-two":
			run["run_attempt"] = 2
		}
		switch r.URL.Path {
		case "/repos/acme/widget/pulls/7":
			count := f.heads.Add(1)
			head, base := "sha", "v5"
			if f.mode == "head" || f.mode == "head-during-read" && count%2 == 0 {
				head = "new-head"
			}
			if f.mode == "retarget-during-read" && count%2 == 0 {
				base = "other-base"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "open",
				"head": map[string]string{"sha": head}, "base": map[string]string{"ref": base}})
		case "/repos/acme/widget/actions/runs":
			if f.beforeRuns != nil {
				f.beforeRuns()
			}
			if f.mode == "forbidden" || f.mode == "unavailable" || f.mode == "rate-limit" {
				status := http.StatusForbidden
				if f.mode == "unavailable" {
					status = http.StatusInternalServerError
				}
				if f.mode == "rate-limit" {
					status = http.StatusTooManyRequests
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"observation unavailable"}`))
				return
			}
			runs := []any{run}
			if f.mode == "newer-active" || f.mode == "newer-success" {
				run["conclusion"] = "cancelled"
				newer := map[string]any{"id": 22, "head_sha": "sha", "workflow_id": 11,
					"run_number": 10, "run_attempt": 1, "event": "pull_request",
					"status": "queued", "conclusion": ""}
				if f.mode == "newer-success" {
					newer["status"], newer["conclusion"] = "completed", "success"
				}
				runs = append(runs, newer)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
		case "/repos/acme/widget/actions/runs/21":
			if f.mode == "attempt-during-read" {
				run["run_attempt"], run["status"] = 2, "queued"
			}
			if f.mode == "run-id-during-read" {
				run["id"] = 99
			}
			_ = json.NewEncoder(w).Encode(run)
		case "/repos/acme/widget/actions/runs/21/attempts/1/jobs", "/repos/acme/widget/actions/runs/21/attempts/2/jobs":
			attempt := 1
			if f.mode == "attempt-two" {
				attempt = 2
			}
			job := map[string]any{"id": 41, "run_id": 21, "run_attempt": attempt,
				"head_sha": "sha", "name": "build-and-test", "status": "queued",
				"check_run_url": server.URL + "/repos/acme/widget/check-runs/51"}
			switch f.mode {
			case "job-head":
				job["head_sha"] = "other-head"
			case "job-attempt":
				job["run_attempt"] = 99
			case "job-run":
				job["run_id"] = 99
			case "job-check":
				job["check_run_url"] = server.URL + "/repos/acme/widget/check-runs/99"
			case "job-provider":
				job["check_run_url"] = "https://other.example/repos/acme/widget/check-runs/51"
			case "failed-job":
				job["status"], job["conclusion"] = "completed", "failure"
			case "unknown-job-status":
				job["status"] = "unrecognized"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []any{job}})
		case "/repos/acme/widget/commits/sha/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"statuses": []any{}})
		case "/repos/acme/widget/commits/sha/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{f.check}})
		default:
			t.Errorf("unexpected observation endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	f.client = gh.NewClient(server.Client())
	f.client.BaseURL, _ = f.client.BaseURL.Parse(server.URL + "/")
	f.check = &gh.CheckRun{ID: gh.Ptr(int64(51)), Name: gh.Ptr("build-and-test"),
		HeadSHA: gh.Ptr("sha"), Status: gh.Ptr("queued"),
		App: &gh.App{Slug: gh.Ptr("github-actions")}, CheckSuite: &gh.CheckSuite{ID: gh.Ptr(int64(31))}}
	return f
}

func TestCIReportingExactRunLifecycle(t *testing.T) {
	cases := []struct{ mode, state string }{
		{"", "stalled"}, {"success", "stalled"}, {"cancelled", "stalled"}, {"failed-job", "stalled"},
		{"active", "active"}, {"old-active", "active"}, {"head", "unverified"},
		{"head-during-read", "unverified"}, {"retarget-during-read", "unverified"},
		{"run-head", "unverified"}, {"run-repo", "unverified"}, {"run-suite", "unverified"},
		{"job-head", "unverified"}, {"job-attempt", "unverified"}, {"job-run", "unverified"},
		{"job-check", "unverified"}, {"job-provider", "unverified"},
		{"unknown-conclusion", "unverified"}, {"unknown-run-status", "unverified"}, {"unknown-job-status", "unverified"}, {"attempt-during-read", "unverified"},
		{"run-id-during-read", "unverified"}, {"newer-active", "superseded"}, {"newer-success", "superseded"},
		{"forbidden", "unverified"}, {"rate-limit", "unverified"}, {"unavailable", "unverified"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			f := newCIReportingFixture(t)
			f.mode = tc.mode
			now := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
			opts := CommitCIOptions{PRNumber: 7, RequiredKnown: true, Now: func() time.Time { return now }}
			cache := newCIReportingCache()
			first := cache.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})
			if tc.state == "stalled" && first[0].State != "propagation-grace" {
				t.Fatalf("terminal age alone declared a stall: %+v", first)
			}
			now = now.Add(defaultCIReportingRecheck)
			observations := cache.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})
			if len(observations) != 1 || observations[0].State != tc.state {
				t.Fatalf("observations=%+v want=%s", observations, tc.state)
			}
			got := observations[0]
			if got.State == "stalled" {
				if got.Repo != "acme/widget" || got.PRNumber != 7 || got.HeadSHA != "sha" || got.RunID != 21 || got.Attempt != 1 || got.JobID != 41 ||
					got.CheckRunID != 51 || got.CheckStatus != "queued" || got.Recovery != "owner-action-required" {
					t.Fatalf("lost provenance or invented recovery: %+v", got)
				}
				if got.State == "active" && got.Recovery != "none" {
					t.Fatalf("ordinary active CI requested recovery: %+v", got)
				}
				reason := ciReportingReason(observations)
				if !strings.HasPrefix(reason, "ci-reporting-stalled:") || stageengine.ClassifyCIReason(reason) != stageengine.CIReasonPending {
					t.Fatalf("reporting mismatch changed stage semantics: %s", reason)
				}
			}
			if strings.Contains(tc.mode, "forbidden") || tc.mode == "rate-limit" || tc.mode == "unavailable" {
				if !strings.HasPrefix(ciReportingReason(observations), "ci-reporting-unverified:") {
					t.Fatalf("API error disappeared: %+v", observations)
				}
			}
		})
	}
}

func TestCIReportingCacheEventsRestartAndAttemptReset(t *testing.T) {
	f := newCIReportingFixture(t)
	now := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	opts := CommitCIOptions{PRNumber: 7, RequiredKnown: true, Now: func() time.Time { return now }}
	cache := newCIReportingCache()
	observe := func() CIReportingObservation {
		return cache.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})[0]
	}
	if observe().State != "propagation-grace" {
		t.Fatal("first observation did not wait")
	}
	reads := f.reads.Load()
	for range 20 {
		cache.invalidate("acme/widget", "sha")
		if observe().State != "propagation-grace" {
			t.Fatal("duplicate event shortened grace")
		}
	}
	if f.reads.Load() != reads {
		t.Fatal("duplicate events caused an API storm")
	}
	now = now.Add(ciReportingEventRecheck)
	if observe().State != "propagation-grace" {
		t.Fatal("early independent sample shortened grace")
	}
	now = now.Add(defaultCIReportingGrace - ciReportingEventRecheck)
	cache.invalidate("acme/widget", "sha")
	if observe().State != "stalled" {
		t.Fatal("consistent terminal/reporting contradiction never became actionable")
	}
	restarted := newCIReportingCache()
	if restarted.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})[0].State != "propagation-grace" {
		t.Fatal("restart assumed a previous observation or authorized a mutation")
	}
	now = now.Add(defaultCIReportingRecheck)
	f.mode = "active"
	if observe().State != "active" {
		t.Fatal("terminal attempt overrode an active rerun")
	}
	now = now.Add(defaultCIReportingRecheck)
	f.mode = "attempt-two"
	got := observe()
	if got.State != "propagation-grace" || got.Attempt != 2 {
		t.Fatalf("new attempt inherited old recovery episode: %+v", got)
	}
	now = now.Add(defaultCIReportingRecheck)
	f.mode = "forbidden"
	if observe().State != "unverified" {
		t.Fatal("permission failure looked normal")
	}
	now = now.Add(defaultCIReportingRecheck)
	f.mode = "attempt-two"
	if observe().State != "propagation-grace" {
		t.Fatal("API gap counted as a consistency sample")
	}
	now = now.Add(-defaultCIReportingRecheck)
	if observe().State != "propagation-grace" {
		t.Fatal("clock reversal promoted a stale episode")
	}
}

func TestCIReportingGateNeverFabricatesGreenAndCommitOnlyUnchanged(t *testing.T) {
	f := newCIReportingFixture(t)
	now := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	previous := sharedCIReportingCache
	sharedCIReportingCache = newCIReportingCache()
	t.Cleanup(func() { sharedCIReportingCache = previous })
	opts := CommitCIOptions{PRNumber: 7, RequiredKnown: true, Required: map[string]bool{"build-and-test": true},
		Now: func() time.Time { return now }}
	for range 2 {
		got, err := EvaluateCommitCI(context.Background(), f.client, "acme", "widget", "sha", opts)
		if err != nil || got.Green || got.InFlight != 1 {
			t.Fatalf("reporting replaced semantic CI: %+v %v", got, err)
		}
		now = now.Add(defaultCIReportingRecheck)
	}
	got, err := EvaluateCommitCI(context.Background(), f.client, "acme", "widget", "sha", opts)
	if err != nil || !strings.HasPrefix(got.Reason, "ci-reporting-stalled:") {
		t.Fatalf("stall was not exposed through the existing reason: %+v %v", got, err)
	}
	opts.PRNumber = 0
	got, err = EvaluateCommitCI(context.Background(), f.client, "acme", "widget", "sha", opts)
	if err != nil || got.Green || got.Reason != "required-check-pending:build-and-test" || len(got.Reporting) != 0 {
		t.Fatalf("base/commit behavior changed: %+v %v", got, err)
	}
	f.check.Status, f.check.Conclusion, f.check.ID = gh.Ptr("completed"), gh.Ptr("success"), gh.Ptr(int64(52))
	opts.PRNumber = 7
	got, err = EvaluateCommitCI(context.Background(), f.client, "acme", "widget", "sha", opts)
	if err != nil || !got.Green || len(got.Reporting) != 0 {
		t.Fatalf("old observation overrode actual newer success: %+v %v", got, err)
	}
}

func TestCIReportingUnsupportedAndBoundedCache(t *testing.T) {
	f := newCIReportingFixture(t)
	f.check.App.Slug = gh.Ptr("external-ci")
	cache := newCIReportingCache()
	now := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	for i := range ciReportingMaxEntries {
		cache.entries[fmt.Sprint(i)] = &ciReportingEntry{checkedAt: now.Add(-time.Duration(i) * time.Minute)}
	}
	opts := CommitCIOptions{PRNumber: 7, RequiredKnown: true, Now: func() time.Time { return now }}
	got := cache.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})
	if len(got) != 1 || got[0].State != "unsupported-provider" || f.reads.Load() != 0 || len(cache.entries) != ciReportingMaxEntries {
		t.Fatalf("unsupported provider polled or unbounded cache: %+v reads=%d entries=%d", got, f.reads.Load(), len(cache.entries))
	}
}

func TestCIReportingConcurrentObserversShareOneRead(t *testing.T) {
	f := newCIReportingFixture(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var arrivals atomic.Int64
	f.beforeRuns = func() {
		if arrivals.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	cache := newCIReportingCache()
	now := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	opts := CommitCIOptions{PRNumber: 7, RequiredKnown: true, Now: func() time.Time { return now }}
	go func() {
		defer close(finished)
		cache.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("observer did not reach the owning workflow read")
	}
	reads := f.reads.Load()
	for range 20 {
		cache.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})
	}
	if f.reads.Load() != reads {
		t.Error("competing observers duplicated workflow reads")
	}
	close(release)
	<-finished
}

func TestCIReportingWorkflowWebhookInvalidatesOnlyObservedHead(t *testing.T) {
	c, _ := setupWebhookCacheTest(t)
	previous := sharedCIReportingCache
	sharedCIReportingCache = newCIReportingCache()
	t.Cleanup(func() { sharedCIReportingCache = previous })
	current := &ciReportingEntry{repo: "org/repo", head: "sha"}
	other := &ciReportingEntry{repo: "org/repo", head: "other"}
	sharedCIReportingCache.entries["current"], sharedCIReportingCache.entries["other"] = current, other
	for range 20 {
		res, err := c.HandleWebhookEvent("workflow_run", []byte(`{"action":"completed","repository":{"full_name":"org/repo"},"workflow_run":{"head_sha":"sha","pull_requests":[{"number":7}]}}`))
		if err != nil || res.HeadSHA != "sha" || len(res.Invalidated) != 1 || res.Invalidated[0] != 7 {
			t.Fatalf("workflow delivery was not propagated: %+v %v", res, err)
		}
	}
	if !current.dirty || other.dirty {
		t.Fatal("workflow event dirtied unrelated immutable heads")
	}
}
