package github

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestWebhookCIFailure_EventsAndConclusions(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload webhookEventPayload
		want    *WebhookCIFailure
	}{
		{name: "check_run failure", event: "check_run", payload: func() webhookEventPayload {
			var p webhookEventPayload
			p.Action = "completed"
			p.CheckRun = ptrCheckRun("abc", "build-gate", "failure", "https://ci/run/1")
			return p
		}(), want: &WebhookCIFailure{HeadSHA: "abc", Check: "build-gate", Conclusion: "failure", DetailsURL: "https://ci/run/1"}},
		{name: "check_run timed_out", event: "check_run", payload: func() webhookEventPayload {
			var p webhookEventPayload
			p.Action = "completed"
			p.CheckRun = ptrCheckRun("abc", "e2e", "timed_out", "")
			return p
		}(), want: &WebhookCIFailure{HeadSHA: "abc", Check: "e2e", Conclusion: "timed_out"}},
		{name: "check_run cancelled is not a failure", event: "check_run", payload: func() webhookEventPayload {
			var p webhookEventPayload
			p.Action = "completed"
			p.CheckRun = ptrCheckRun("abc", "e2e", "cancelled", "")
			return p
		}()},
		{name: "check_run created is not a failure", event: "check_run", payload: func() webhookEventPayload {
			var p webhookEventPayload
			p.Action = "created"
			p.CheckRun = ptrCheckRun("abc", "e2e", "failure", "")
			return p
		}()},
		{name: "status error", event: "status", payload: webhookEventPayload{SHA: "def", State: "error", Context: "ci/lint", TargetURL: "https://ci/2"},
			want: &WebhookCIFailure{HeadSHA: "def", Check: "ci/lint", Conclusion: "error", DetailsURL: "https://ci/2"}},
		{name: "status pending", event: "status", payload: webhookEventPayload{SHA: "def", State: "pending", Context: "ci/lint"}},
		{name: "check_suite success", event: "check_suite", payload: func() webhookEventPayload {
			var p webhookEventPayload
			p.Action = "completed"
			p.CheckSuite = ptrCheckSuite("abc", "success")
			return p
		}()},
		{name: "check_suite failure", event: "check_suite", payload: func() webhookEventPayload {
			var p webhookEventPayload
			p.Action = "completed"
			p.CheckSuite = ptrCheckSuite("abc", "failure")
			return p
		}(), want: &WebhookCIFailure{HeadSHA: "abc", Check: "GitHub Actions check suite", Conclusion: "failure"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := webhookCIFailure(tc.event, tc.payload)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got == nil {
				return
			}
			if got.HeadSHA != tc.want.HeadSHA || got.Check != tc.want.Check || got.Conclusion != tc.want.Conclusion || got.DetailsURL != tc.want.DetailsURL {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

type checkRunPayload = struct {
	HeadSHA      string         `json:"head_sha"`
	Name         string         `json:"name"`
	Conclusion   string         `json:"conclusion"`
	DetailsURL   string         `json:"details_url"`
	HTMLURL      string         `json:"html_url"`
	PullRequests []webhookPRRef `json:"pull_requests"`
}

type checkSuitePayload = struct {
	HeadSHA      string         `json:"head_sha"`
	Conclusion   string         `json:"conclusion"`
	URL          string         `json:"url"`
	PullRequests []webhookPRRef `json:"pull_requests"`
	App          struct {
		Name string `json:"name"`
	} `json:"app"`
}

func ptrCheckRun(sha, name, conclusion, url string) *checkRunPayload {
	return &checkRunPayload{HeadSHA: sha, Name: name, Conclusion: conclusion, DetailsURL: url}
}

func ptrCheckSuite(sha, conclusion string) *checkSuitePayload {
	p := &checkSuitePayload{HeadSHA: sha, Conclusion: conclusion}
	p.App.Name = "GitHub Actions"
	return p
}

// A failed check_run on a cached PR head resolves to the PR with its author
// and hive-authorship captured BEFORE the delivery invalidates the entry; a
// second delivery for the same head finds the cache empty and reports the
// PR by number only (the sweep fills in the rest).
func TestHandleWebhookEvent_CIFailureResolvesCachedPR(t *testing.T) {
	t.Cleanup(resetPRDetailCacheForTest(nil, 0))
	t.Cleanup(ResetWebhookTrackerForTest())
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("webhook must not call GitHub: %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	c.SetAppBotLogin("hive-bot[bot]")

	sharedPRDetailCache.put("acme/widgets", 41, &gh.PullRequest{
		Number: gh.Ptr(41), State: gh.Ptr("open"), Title: gh.Ptr("agent fix"),
		User: &gh.User{Login: gh.Ptr("hive-bot[bot]")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("feedface"), Ref: gh.Ptr("hive/fix")},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("main")},
	})
	sharedPRDetailCache.put("acme/widgets", 42, &gh.PullRequest{
		Number: gh.Ptr(42), State: gh.Ptr("open"), Title: gh.Ptr("human PR"),
		User: &gh.User{Login: gh.Ptr("octocat")},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("c0ffee"), Ref: gh.Ptr("feat")},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("main")},
	})

	body := []byte(`{"action":"completed","repository":{"full_name":"acme/widgets"},
	  "check_run":{"head_sha":"feedface","name":"build-gate","conclusion":"failure","details_url":"https://ci/1","pull_requests":[]}}`)
	res, err := c.HandleWebhookEvent("check_run", body)
	if err != nil {
		t.Fatal(err)
	}
	if res.CIFailure == nil || len(res.CIFailure.PRs) != 1 {
		t.Fatalf("CIFailure = %+v, want one PR", res.CIFailure)
	}
	pr := res.CIFailure.PRs[0]
	if pr.Number != 41 || !pr.HiveAuthored || pr.Author != "hive-bot[bot]" || pr.Title != "agent fix" || pr.BaseRef != "main" {
		t.Fatalf("resolved PR = %+v", pr)
	}
	if res.CIFailure.Check != "build-gate" || res.CIFailure.DetailsURL != "https://ci/1" {
		t.Fatalf("CIFailure = %+v", res.CIFailure)
	}

	// Contributor PR: resolved, not hive-authored.
	body = []byte(`{"sha":"c0ffee","state":"failure","context":"ci/test","target_url":"https://ci/2","repository":{"full_name":"acme/widgets"}}`)
	res, err = c.HandleWebhookEvent("status", body)
	if err != nil {
		t.Fatal(err)
	}
	if res.CIFailure == nil || len(res.CIFailure.PRs) != 1 || res.CIFailure.PRs[0].HiveAuthored || res.CIFailure.PRs[0].Number != 42 {
		t.Fatalf("contributor CIFailure = %+v", res.CIFailure)
	}

	// Success deliveries carry no failure.
	body = []byte(`{"action":"completed","repository":{"full_name":"acme/widgets"},
	  "check_run":{"head_sha":"feedface","name":"build-gate","conclusion":"success","pull_requests":[{"number":41}]}}`)
	res, _ = c.HandleWebhookEvent("check_run", body)
	if res.CIFailure != nil {
		t.Fatalf("success must not produce a CI failure: %+v", res.CIFailure)
	}

	// Unknown head (cache already invalidated): the delivery still names the
	// PR when GitHub lists it, by number only.
	body = []byte(`{"action":"completed","repository":{"full_name":"acme/widgets"},
	  "check_run":{"head_sha":"feedface","name":"lint","conclusion":"failure","pull_requests":[{"number":41}]}}`)
	res, _ = c.HandleWebhookEvent("check_run", body)
	if res.CIFailure == nil || len(res.CIFailure.PRs) != 1 || res.CIFailure.PRs[0].Author != "" {
		t.Fatalf("number-only CIFailure = %+v", res.CIFailure)
	}
	// And a head no PR matches is an unmatched failure.
	body = []byte(`{"sha":"deadbeef","state":"failure","context":"ci/test","repository":{"full_name":"acme/widgets"}}`)
	res, _ = c.HandleWebhookEvent("status", body)
	if res.CIFailure == nil || len(res.CIFailure.PRs) != 0 {
		t.Fatalf("unmatched CIFailure = %+v", res.CIFailure)
	}
}

func TestCIFailureSignalStore_DedupCooldownAndSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	t.Cleanup(ResetCIFailureSignalsForTest(clock))
	s := CIFailureSignals()

	sig := CIFailureSignal{Repo: "acme/widgets", Number: 7, HeadSHA: "ABC", Check: "build"}
	if !s.Record(sig) {
		t.Fatal("first signal must be fresh")
	}
	sig.HeadSHA = "abc" // same head, case-insensitive
	if s.Record(sig) {
		t.Fatal("same head+check must dedup")
	}
	sig.Check = "lint"
	if !s.Record(sig) {
		t.Fatal("different check on the same head is a new signal")
	}
	if got := s.Snapshot(); got.Count != 2 || got.Last == nil || got.Last.Check != "lint" {
		t.Fatalf("snapshot = %+v", got)
	}

	if !s.AllowKick("acme/widgets", 7, "abc", 10*time.Minute) {
		t.Fatal("first kick must be allowed")
	}
	now = now.Add(5 * time.Minute)
	if s.AllowKick("acme/widgets", 7, "abc", 10*time.Minute) {
		t.Fatal("kick inside the cooldown must be refused")
	}
	if !s.AllowKick("acme/widgets", 7, "newhead", 10*time.Minute) {
		t.Fatal("a new head is outside the cooldown")
	}
	now = now.Add(6 * time.Minute)
	if !s.AllowKick("acme/widgets", 7, "abc", 10*time.Minute) {
		t.Fatal("kick after the cooldown must be allowed")
	}
	if got := s.Snapshot().Kicks; got != 3 {
		t.Fatalf("kicks = %d, want 3", got)
	}

	s.SetAgent("acme/widgets", 7, "scanner")
	if got := s.ForAgent("scanner"); len(got) != 2 || got[0].Agent != "scanner" {
		t.Fatalf("ForAgent = %+v", got)
	}
	if got := s.ForPR("ACME/widgets", 7); len(got) != 2 {
		t.Fatalf("ForPR = %+v", got)
	}
	s.Forget("acme/widgets", 7)
	if got := s.Snapshot().Count; got != 0 {
		t.Fatalf("count after Forget = %d", got)
	}

	s.Record(CIFailureSignal{Repo: "acme/widgets", Number: 8, HeadSHA: "x", Check: "build"})
	now = now.Add(ciFailureSignalTTL + time.Minute)
	if got := s.Snapshot().Count; got != 0 {
		t.Fatalf("signal must expire after the TTL, count = %d", got)
	}
}
