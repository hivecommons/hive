package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// TestStageTrackerCICountsFromWebhookEvidence: check deliveries applied to
// the per-head evidence store drive the CI chevron counts for a PR whose
// status snapshot carries no (or stale) CIActivity — including sidelined
// PRs the eval cycle never enriches (hivecommons/hive#16521).
func TestStageTrackerCICountsFromWebhookEvidence(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetCIEvidenceForTest(func() time.Time { return now }))
	s, store := stageTrackerFixture(t, now)

	appID := int64(15368)
	done := now.Add(-time.Minute)
	checks := []github.PRActivityCheck{
		{Name: "build", State: "completed", Conclusion: "success", NodeID: "CR_build", AppID: &appID, Since: &done},
		{Name: "test", State: "in_progress", NodeID: "CR_test", AppID: &appID, Since: &done},
	}
	if n := github.RecordCIEvidenceForTest("acme/console", "sha-103", checks); n != 2 {
		t.Fatalf("recorded %d checks, want 2", n)
	}
	if n := github.RecordCIEvidenceForTest("acme/console", "sha-106", checks[:1]); n != 1 {
		t.Fatalf("recorded %d checks, want 1", n)
	}

	s.statusMu.Lock()
	repo := &s.status.Repos[0]
	for i, raw := range repo.OpenPrs {
		pr, ok := raw.(FrontendPR)
		if !ok || pr.Number != 103 {
			continue
		}
		pr.HeadSHA = "sha-103"
		// Evidence from a previous head must not win over the new head.
		stale := now.Add(-time.Hour)
		pr.CIActivity = &github.PRCIActivity{Head: "old-103", Source: "github-check-runs", ObservedAt: &stale,
			Checks: []github.PRActivityCheck{{Name: "build", State: "completed", Conclusion: "failure"}}}
		repo.OpenPrs[i] = pr
	}
	repo.NonActionablePrs = []any{
		github.RepoWorkPR{Repo: "acme/console", Number: 106, Title: "WIP draft", Author: "zoe", Draft: true, Bucket: "draft", Reason: "Draft", HeadSHA: "sha-106", CreatedAt: now.Add(-24 * time.Hour)},
	}
	s.statusMu.Unlock()

	resp := s.buildStageTracker(s.status, store, now)
	pr103 := trackerItem(t, resp, "pr", 103)
	if pr103.CIActivity == nil || pr103.CIActivity.Head != "sha-103" || pr103.CIActivity.ChecksTotal != 2 || pr103.CIActivity.ChecksPassed != 1 || pr103.CIActivity.ChecksFailed != 0 {
		t.Fatalf("pr103 CI = %+v, want 1/2 on sha-103 from webhook evidence", pr103.CIActivity)
	}
	pr106 := trackerItem(t, resp, "pr", 106)
	if pr106.CIActivity == nil || pr106.CIActivity.ChecksTotal != 1 || pr106.CIActivity.ChecksPassed != 1 {
		t.Fatalf("sidelined pr106 CI = %+v, want 1/1 from webhook evidence", pr106.CIActivity)
	}
}

// TestGitHubWebhookCheckRunAppliesCIEvidenceWithoutGitHubCall: a
// `check_run` delivery that names its PR updates the CI evidence from the
// payload alone — no PR re-read is queued, the GitHub test server would
// fail the test on any call — and /api/status.webhooks reports CI event
// health.
func TestGitHubWebhookCheckRunAppliesCIEvidenceWithoutGitHubCall(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	t.Cleanup(github.ResetCIEvidenceForTest(func() time.Time { return now }))
	s := newGitHubWebhookTestServer(t)

	// A prior listing exists for the head, so the delivery needs no re-read.
	appID := int64(15368)
	started := now.Add(-2 * time.Minute)
	listedAt := now.Add(-time.Minute)
	github.RecordCIListingForTest("webhookorg/webhookrepo", "abc123", &github.PRCIActivity{
		Head: "abc123", Source: "github-check-runs", ObservedAt: &listedAt,
		Checks: []github.PRActivityCheck{{Name: "unit", State: "in_progress", NodeID: "CR_unit", AppID: &appID, Since: &started}},
	})

	body := []byte(`{"action":"completed","repository":{"full_name":"webhookorg/webhookrepo"},
		"check_run":{"id":1,"node_id":"CR_unit","name":"unit","status":"completed","conclusion":"success","head_sha":"abc123",
			"started_at":"2026-10-17T08:58:00Z","completed_at":"2026-10-17T08:59:30Z","app":{"id":15368,"name":"GitHub Actions"},
			"pull_requests":[{"number":5}]}}`)
	rec := postGitHubWebhook(s, "check_run", signGitHubWebhookForTest(secret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Status         string `json:"status"`
		TrackerRefresh *int   `json:"tracker_refresh"`
		CIEvidence     *struct {
			HeadSHA string `json:"head_sha"`
			Applied int    `json:"applied"`
			Refetch bool   `json:"refetch"`
			PRs     []int  `json:"prs"`
		} `json:"ci_evidence"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.CIEvidence == nil || got.CIEvidence.Applied != 1 || got.CIEvidence.Refetch || got.CIEvidence.HeadSHA != "abc123" || len(got.CIEvidence.PRs) != 1 || got.CIEvidence.PRs[0] != 5 {
		t.Fatalf("response = %s, want ci_evidence applied 1 on abc123 for PR 5 with no refetch", rec.Body.String())
	}
	if got.TrackerRefresh != nil {
		t.Fatalf("payload-served check delivery must not queue a PR re-read: %s", rec.Body.String())
	}
	completed := now.Add(-30 * time.Second)
	merged := github.HeadCIActivity("webhookorg/webhookrepo", "abc123", nil)
	if merged == nil || len(merged.Checks) != 1 || merged.Checks[0].Conclusion != "success" || merged.Checks[0].Since == nil || !merged.Checks[0].Since.Equal(completed) {
		t.Fatalf("head evidence = %+v, want the completed unit run", merged)
	}
	pr := github.PullRequest{Number: 5, HeadSHA: "abc123"}
	if ci := stageTrackerCIActivity(github.PullRequest{Number: 5, HeadSHA: "abc123", CIActivity: merged}, now); ci.ChecksTotal != 1 || ci.ChecksPassed != 1 {
		t.Fatalf("tracker counts for %d = %+v, want 1/1", pr.Number, ci)
	}

	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "webhook-test"}
	s.statusMu.Unlock()
	statusRec := httptest.NewRecorder()
	s.handleStatus(statusRec, httptest.NewRequest(http.MethodGet, "/api/status?fields=webhooks", bytes.NewReader(nil)))
	var status struct {
		Webhooks struct {
			LastCIEventAt     *time.Time `json:"last_ci_event_at"`
			CIEvents1h        int        `json:"ci_events_1h"`
			CIEventsApplied1h int        `json:"ci_events_applied_1h"`
			CIRefetches1h     int        `json:"ci_refetches_1h"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	w := status.Webhooks
	if w.LastCIEventAt == nil || !w.LastCIEventAt.Equal(now) || w.CIEvents1h != 1 || w.CIEventsApplied1h != 1 || w.CIRefetches1h != 0 {
		t.Fatalf("/api/status webhooks CI health = %+v, want one event applied at %s", w, now)
	}
}
