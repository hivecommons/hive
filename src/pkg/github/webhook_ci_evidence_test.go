package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func ciTestCheckRunBody(number int, head, name, status, conclusion string, at time.Time) []byte {
	completed := "null"
	if status == "completed" {
		completed = fmt.Sprintf("%q", at.Format(time.RFC3339))
	}
	return []byte(fmt.Sprintf(`{"action":%q,"repository":{"full_name":"org/repo"},"check_run":{"id":%d,"node_id":"CR_%s_%d","head_sha":%q,"name":%q,"status":%q,"conclusion":%q,"details_url":"https://ci/%s","started_at":%q,"completed_at":%s,"pull_requests":[{"number":%d}],"app":{"id":15368,"name":"GitHub Actions"}}}`,
		status, len(name)*100+number, name, len(name), head, name, status, conclusion, name, at.Add(-time.Minute).Format(time.RFC3339), completed, number))
}

type ciTestTimer struct{}

func (t *ciTestTimer) Stop() bool { return true }

// A check_run delivery is applied to the head's check set from the payload
// alone — no GitHub call — and the merged view the tracker reads counts it
// at once; the head's one coalesced re-read is queued because no listing
// has covered it yet.
func TestHandleWebhookEventCheckRunAppliesCIEvidenceFromPayload(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	t.Cleanup(ResetCIEvidenceForTest(clock.Now))
	var fired func()
	c.ciRefetch = &ciRefetcher{newTimer: func(_ time.Duration, f func()) ciRefetchTimer { fired = f; return &ciTestTimer{} }}
	c.storePRDetail("repo", 7, testPRDetail(7, "sha-7", clock.now, "clean"))

	res, err := c.HandleWebhookEvent("check_run", ciTestCheckRunBody(7, "sha-7", "unit", "in_progress", "", clock.now))
	if err != nil {
		t.Fatal(err)
	}
	if res.CIEvidence == nil || res.CIEvidence.Applied != 1 || !res.CIEvidence.Refetch || !reflect.DeepEqual(res.CIEvidence.PRs, []int{7}) || res.CIEvidence.HeadSHA != "sha-7" {
		t.Fatalf("ci_evidence = %+v, want applied 1, refetch, PRs [7]", res.CIEvidence)
	}
	if fired == nil || c.ciRefetch.pendingCount() != 1 {
		t.Fatalf("expected one pending head re-read, pending=%d timer armed=%v", c.ciRefetch.pendingCount(), fired != nil)
	}

	clock.now = clock.now.Add(30 * time.Second)
	if _, err := c.HandleWebhookEvent("check_run", ciTestCheckRunBody(7, "sha-7", "unit", "completed", "success", clock.now)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HandleWebhookEvent("check_run", ciTestCheckRunBody(7, "sha-7", "lint", "completed", "failure", clock.now)); err != nil {
		t.Fatal(err)
	}
	if c.ciRefetch.pendingCount() != 1 {
		t.Fatalf("fan-out must coalesce to one pending head, got %d", c.ciRefetch.pendingCount())
	}

	got := HeadCIActivity("org/repo", "sha-7", nil)
	if got == nil || got.Source != "github-webhook" || got.ObservedAt == nil || !got.ObservedAt.Equal(clock.now) {
		t.Fatalf("merged activity = %+v, want webhook-sourced, observed at the last delivery", got)
	}
	want := map[string]string{"unit": "completed/success", "lint": "completed/failure"}
	if len(got.Checks) != len(want) {
		t.Fatalf("checks = %+v, want %v", got.Checks, want)
	}
	for _, check := range got.Checks {
		if want[check.Name] != check.State+"/"+check.Conclusion {
			t.Fatalf("check %s = %s/%s, want %s", check.Name, check.State, check.Conclusion, want[check.Name])
		}
		if check.Head != "sha-7" || check.AppID == nil || *check.AppID != githubActionsAppID {
			t.Fatalf("check %+v must carry the head and producing App", check)
		}
	}
	// A bare configured name resolves to the same head.
	if bare := HeadCIActivity("repo", "sha-7", nil); bare == nil || len(bare.Checks) != 2 {
		t.Fatalf("bare repo lookup = %+v", bare)
	}
	// An out-of-order re-delivery of the older in_progress run must not
	// roll the completed result back.
	if _, err := c.HandleWebhookEvent("check_run", ciTestCheckRunBody(7, "sha-7", "unit", "in_progress", "", clock.now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	for _, check := range HeadCIActivity("org/repo", "sha-7", nil).Checks {
		if check.Name == "unit" && check.Conclusion != "success" {
			t.Fatalf("stale re-delivery rolled unit back: %+v", check)
		}
	}
	health := WebhookHealthSnapshot()
	if health.CIEvents1h != 4 || health.CIEventsApplied1h != 4 || health.LastCIEventAt == nil || health.CIRefetches1h != 0 {
		t.Fatalf("webhook health CI block = %+v", health)
	}
}

// Delivered checks lay over the cycle's listing: a newer delivery replaces
// the listed run of the same check, the required set the listing resolved
// is kept, and a listing taken after the deliveries supersedes them.
func TestHeadCIActivityMergesDeliveriesOverListing(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(ResetCIEvidenceForTest(clock.Now))
	t.Cleanup(resetWebhookTrackerForTest(clock.Now))
	listedAt := clock.now
	appID := githubActionsAppID
	listed := &PRCIActivity{Head: "sha-1", Source: "github-check-runs", ObservedAt: &listedAt,
		RequiredKnown: true, Required: []string{"lint", "unit"}, RequiredSource: "protection",
		RequiredChecks: []PRRequiredCheck{{Name: "lint", AppKnown: true, AppID: &appID}, {Name: "unit", AppKnown: true, AppID: &appID}},
		Checks: []PRActivityCheck{
			{Name: "lint", State: "completed", Conclusion: "success", NodeID: "CR_lint", AppID: &appID, Head: "sha-1"},
			{Name: "unit", State: "in_progress", NodeID: "CR_unit", AppID: &appID, Head: "sha-1"},
		}}
	sharedCIEvidence().recordListed("org/repo", "sha-1", listed)

	// The tracker's base (the status snapshot's copy) is the same listing.
	if got := HeadCIActivity("org/repo", "sha-1", listed); !reflect.DeepEqual(got, listed) {
		t.Fatalf("no deliveries: merged = %+v, want the listing unchanged", got)
	}

	clock.now = clock.now.Add(time.Minute)
	done := clock.now
	n := RecordCIEvidenceForTest("org/repo", "sha-1", []PRActivityCheck{{Name: "unit", State: "completed", Conclusion: "success", NodeID: "CR_unit", AppID: &appID, Since: &done}})
	if n != 1 {
		t.Fatalf("applied = %d", n)
	}
	got := HeadCIActivity("org/repo", "sha-1", listed)
	if got.Source != "github-check-runs+github-webhook" || !got.RequiredKnown || !reflect.DeepEqual(got.Required, []string{"lint", "unit"}) {
		t.Fatalf("merged = %+v, want listing's required set with webhook source", got)
	}
	if len(got.Checks) != 2 {
		t.Fatalf("checks = %+v, want unit replaced not appended", got.Checks)
	}
	for _, check := range got.Checks {
		if check.Name == "unit" && (check.State != "completed" || check.Conclusion != "success") {
			t.Fatalf("unit = %+v, want the delivered completion", check)
		}
	}
	if len(listed.Checks) != 2 || listed.Checks[1].State != "in_progress" {
		t.Fatalf("merge mutated the caller's base: %+v", listed.Checks)
	}
	if got.ObservedAt == nil || !got.ObservedAt.Equal(done) {
		t.Fatalf("observed_at = %v, want the delivery time %v", got.ObservedAt, done)
	}

	// A fresher listing (the next cycle) carries the delivered result
	// itself; the stored deliveries are dropped and the listing wins.
	clock.now = clock.now.Add(time.Minute)
	relistedAt := clock.now
	relisted := cloneCIActivity(listed)
	relisted.ObservedAt = &relistedAt
	relisted.Checks[1] = PRActivityCheck{Name: "unit", State: "completed", Conclusion: "failure", NodeID: "CR_unit", AppID: &appID, Head: "sha-1"}
	sharedCIEvidence().recordListed("org/repo", "sha-1", relisted)
	if got := HeadCIActivity("org/repo", "sha-1", nil); !strings.HasSuffix(got.Source, "github-check-runs") || got.Checks[1].Conclusion != "failure" {
		t.Fatalf("after re-listing merged = %+v, want the listing alone", got)
	}
	// A stale base from the snapshot (older than the store's listing) does
	// not win over it either.
	if got := HeadCIActivity("org/repo", "sha-1", listed); got.Checks[1].Conclusion != "failure" {
		t.Fatalf("older base won over the newer listing: %+v", got.Checks)
	}
	// Another head is untouched.
	if got := HeadCIActivity("org/repo", "sha-2", nil); got != nil {
		t.Fatalf("other head = %+v, want nil", got)
	}
}

// A `status` delivery becomes a commit_status observation (the legacy
// context side the required-check merge reads), and a `workflow_job`
// delivery is the Actions check run it describes.
func TestWebhookCIChecksStatusAndWorkflowJob(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	t.Cleanup(ResetCIEvidenceForTest(clock.Now))
	c.ciRefetch = &ciRefetcher{newTimer: func(_ time.Duration, f func()) ciRefetchTimer { return &ciTestTimer{} }}
	c.storePRDetail("repo", 3, testPRDetail(3, "sha-3", clock.now, "clean"))

	status := []byte(`{"id":91,"node_id":"SC_91","sha":"sha-3","state":"success","context":"dco","target_url":"https://dco/3","created_at":"2026-10-08T12:00:00Z","repository":{"full_name":"org/repo"}}`)
	res, err := c.HandleWebhookEvent("status", status)
	if err != nil || res.CIEvidence == nil || res.CIEvidence.Applied != 1 {
		t.Fatalf("status delivery: res=%+v err=%v", res.CIEvidence, err)
	}
	job := []byte(`{"action":"completed","repository":{"full_name":"org/repo"},"workflow_job":{"id":501,"node_id":"WJ_501","head_sha":"sha-3","name":"gate","status":"completed","conclusion":"success","html_url":"https://gh/job/501","started_at":"2026-10-08T11:58:00Z","completed_at":"2026-10-08T12:00:00Z"}}`)
	res, err = c.HandleWebhookEvent("workflow_job", job)
	if err != nil || res.CIEvidence == nil || res.CIEvidence.Applied != 1 || !reflect.DeepEqual(res.Invalidated, []int{3}) {
		t.Fatalf("workflow_job delivery: res=%+v invalidated=%v err=%v", res.CIEvidence, res.Invalidated, err)
	}
	got := HeadCIActivity("org/repo", "sha-3", nil)
	if got == nil || len(got.Checks) != 2 {
		t.Fatalf("merged = %+v", got)
	}
	byName := map[string]PRActivityCheck{}
	for _, check := range got.Checks {
		byName[check.Name] = check
	}
	if dco := byName["dco"]; dco.Kind != "commit_status" || dco.State != "completed" || dco.Conclusion != "success" || dco.NodeID != "SC_91" {
		t.Fatalf("dco = %+v", dco)
	}
	if gate := byName["gate"]; gate.AppID == nil || *gate.AppID != githubActionsAppID || gate.Conclusion != "success" || gate.URL != "https://gh/job/501" {
		t.Fatalf("gate = %+v", gate)
	}
	// check_suite carries no per-check result: nothing applied, head kept
	// pending for its re-read, and a non-CI event reports no evidence.
	suite := []byte(`{"action":"completed","repository":{"full_name":"org/repo"},"check_suite":{"head_sha":"sha-3","conclusion":"success","pull_requests":[{"number":3}],"app":{"name":"GitHub Actions"}}}`)
	res, err = c.HandleWebhookEvent("check_suite", suite)
	if err != nil || res.CIEvidence == nil || res.CIEvidence.Applied != 0 || !res.CIEvidence.Refetch {
		t.Fatalf("check_suite delivery: res=%+v err=%v", res.CIEvidence, err)
	}
	pr := []byte(`{"action":"synchronize","number":3,"pull_request":{"number":3,"state":"open"},"repository":{"full_name":"org/repo"}}`)
	if res, err := c.HandleWebhookEvent("pull_request", pr); err != nil || res.CIEvidence != nil {
		t.Fatalf("pull_request delivery must carry no CI evidence: %+v err=%v", res.CIEvidence, err)
	}
}

// The coalesced re-read lists the head's check runs once for any number
// of deliveries, records the result as the head's listing (so later
// deliveries lay over it) and tells the hook; its REST caller tag is its
// own. Required checks are resolved only when the PR's base is known.
func TestWebhookCIRefetchListsHeadOnceAndRecordsListing(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetPRDetailCacheForTest(clock.Now, 0))
	t.Cleanup(resetWebhookTrackerForTest(clock.Now))
	t.Cleanup(ResetCIEvidenceForTest(clock.Now))
	listings := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/repo/commits/sha-9/check-runs" {
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		listings++
		_ = json.NewEncoder(w).Encode(gh.ListCheckRunsResults{Total: gh.Ptr(2), CheckRuns: []*gh.CheckRun{
			{ID: gh.Ptr(int64(1)), NodeID: gh.Ptr("CR_1"), Name: gh.Ptr("unit"), Status: gh.Ptr("completed"), Conclusion: gh.Ptr("success"), HeadSHA: gh.Ptr("sha-9"), App: &gh.App{ID: gh.Ptr(githubActionsAppID)}},
			{ID: gh.Ptr(int64(2)), NodeID: gh.Ptr("CR_2"), Name: gh.Ptr("lint"), Status: gh.Ptr("in_progress"), HeadSHA: gh.Ptr("sha-9"), App: &gh.App{ID: gh.Ptr(githubActionsAppID)}},
		}})
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	var fired func()
	c.ciRefetch = &ciRefetcher{newTimer: func(_ time.Duration, f func()) ciRefetchTimer { fired = f; return &ciTestTimer{} }}
	c.storePRDetail("repo", 9, testPRDetail(9, "sha-9", clock.now, "clean"))
	var hooked []string
	SetCIEvidenceHook(func(repo, head string) { hooked = append(hooked, repo+"@"+head) })
	t.Cleanup(func() { SetCIEvidenceHook(nil) })

	suite := []byte(`{"action":"requested","repository":{"full_name":"org/repo"},"check_suite":{"head_sha":"sha-9","pull_requests":[{"number":9}],"app":{"name":"GitHub Actions"}}}`)
	for i := 0; i < 3; i++ {
		if _, err := c.HandleWebhookEvent("check_suite", suite); err != nil {
			t.Fatal(err)
		}
	}
	if c.ciRefetch.pendingCount() != 1 || fired == nil {
		t.Fatalf("pending = %d, want 1 coalesced head", c.ciRefetch.pendingCount())
	}
	fired()
	if listings != 1 {
		t.Fatalf("check-run listings = %d, want 1", listings)
	}
	if !sharedCIEvidence().hasListed("org/repo", "sha-9") || c.ciRefetch.pendingCount() != 0 {
		t.Fatal("re-read did not record the head's listing")
	}
	if !reflect.DeepEqual(hooked, []string{"org/repo@sha-9"}) {
		t.Fatalf("hook calls = %v", hooked)
	}
	got := HeadCIActivity("org/repo", "sha-9", nil)
	if got == nil || got.Source != "github-check-runs" || len(got.Checks) != 2 || got.RequiredKnown {
		t.Fatalf("listing = %+v, want two listed checks, required unknown without a base ref", got)
	}
	if WebhookHealthSnapshot().CIRefetches1h != 1 {
		t.Fatalf("health = %+v, want one CI refetch", WebhookHealthSnapshot())
	}
	// Now that a listing exists, a delivery lays over it without queuing
	// another re-read.
	clock.now = clock.now.Add(time.Minute)
	res, err := c.HandleWebhookEvent("check_run", ciTestCheckRunBody(9, "sha-9", "lint", "completed", "success", clock.now))
	if err != nil || res.CIEvidence.Refetch || c.ciRefetch.pendingCount() != 0 {
		t.Fatalf("listed head must not re-read: %+v pending=%d err=%v", res.CIEvidence, c.ciRefetch.pendingCount(), err)
	}
	for _, check := range HeadCIActivity("org/repo", "sha-9", nil).Checks {
		if check.Name == "lint" && check.Conclusion != "success" {
			t.Fatalf("lint = %+v, want delivered completion over the listing", check)
		}
	}
	if !strings.Contains(hooked[len(hooked)-1], "sha-9") || len(hooked) != 2 {
		t.Fatalf("hook calls = %v, want the applied delivery to notify too", hooked)
	}
	// A context already cancelled makes the flush a no-op rather than a call.
	c.ciRefetch.ctx = func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	c.ciRefetch.enqueue(ciRefetchTarget{repo: "repo", fullName: "org/repo", head: "sha-9", number: 9})
	c.FlushWebhookCIRefetch()
	if listings != 1 {
		t.Fatalf("cancelled context still listed: %d", listings)
	}
}
