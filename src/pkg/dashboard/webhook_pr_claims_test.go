package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#14814 regression suite: an owner PR opened with
// `Fixes #N` between two reconcile passes left #N offerable for up to an
// hour, and the contribute lane handed #14765 to a contributor nine minutes
// after #14772 (Fixes #14765) opened. The `pull_request` webhook now records
// the claim into the duplicate-PR ledger on the event, and selectTask
// (which reads that ledger through Dependencies.IssueClaimed) stops
// offering the issue at once. An `issues` closed delivery already evicts
// the row from the status payload selectTask offers from; the second test
// pins that path for the same incident.

// webhookClaimFixture wires the dashboard server the way the governor does:
// the webhook receiver's claim hook writes a real ledger, and the contribute
// admission gate reads the same ledger.
func webhookClaimFixture(t *testing.T) (*ContributeWSHub, *Server, *ghpkg.ClaimLedger) {
	t.Helper()
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	hub, s := covK2Hub(t)
	// Typed items, as the governor publishes them (the map shape other
	// contribute fixtures use is a JSON round-trip artefact).
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "dakota", Full: "projectbluefin/dakota",
		ActionableIssues: []any{
			ghpkg.Issue{Repo: "dakota", Number: 353, Title: "the issue everyone kept fixing", URL: "https://github.com/projectbluefin/dakota/issues/353", Author: "someone"},
			ghpkg.Issue{Repo: "dakota", Number: 360, Title: "an unclaimed issue", URL: "https://github.com/projectbluefin/dakota/issues/360", Author: "someone"},
		},
	}}}
	s.statusMu.Unlock()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// selectTask's trust check reads the contributor's permission;
		// the webhook receiver itself must not call GitHub.
		if strings.Contains(r.URL.Path, "/collaborators/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": "read"})
			return
		}
		t.Errorf("webhook receiver must not call GitHub: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ghSrv.Close)
	s.deps.GHClient = ghpkg.NewClientForTest(ghSrv.URL, "projectbluefin", []string{"dakota"}, s.logger)
	s.deps.Ctx = context.Background()
	timers := &fakeTimers{}
	s.webhookRefresh = newWebhookTrackerRefresher()
	s.webhookRefresh.newTimer = timers.newTimer

	ledger := ghpkg.NewClaimLedger(filepath.Join(t.TempDir(), "claims.json"), s.logger)
	ghpkg.SetPRClaimsHook(func(d ghpkg.PRClaimDelivery) { ledger.ApplyPRDelivery(d) })
	t.Cleanup(func() { ghpkg.SetPRClaimsHook(nil) })
	s.deps.IssueClaimed = ledger.Lookup
	return hub, s, ledger
}

func postSignedWebhook(t *testing.T, s *Server, event string, body []byte) map[string]any {
	t.Helper()
	rec := postGitHubWebhook(s, event, signGitHubWebhookForTest("s3cr3t", body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d (%s)", event, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWebhookPRClaimStopsSelectTaskOffering(t *testing.T) {
	hub, s, ledger := webhookClaimFixture(t)

	// Baseline: nothing claimed, the first issue is offered.
	if msg := hub.selectTask(claimedIssueConn()); msg == nil || msg.Type != "task_assign" || msg.Number != 353 {
		t.Fatalf("baseline offer = %+v, want #353", msg)
	}

	// An owner PR opens with `Fixes #353`. The claim must land before any
	// reconcile pass.
	body := []byte(`{"action":"opened","repository":{"full_name":"projectbluefin/dakota"},"pull_request":{"number":900,"state":"open","title":"fix it","body":"Fixes #353","html_url":"https://github.com/projectbluefin/dakota/pull/900","user":{"login":"castrojo"},"head":{"ref":"fix-353","sha":"abc"}}}`)
	resp := postSignedWebhook(t, s, "pull_request", body)
	claimed, _ := resp["claimed_issues"].([]any)
	if len(claimed) != 1 || claimed[0] != float64(353) {
		t.Fatalf("response claimed_issues = %v, want [353]", resp["claimed_issues"])
	}
	if c, ok := ledger.Lookup("dakota", 353); !ok || c.PRNumber != 900 || !c.ExternalAuthor {
		t.Fatalf("ledger claim = %+v ok=%v, want external claim by PR 900", c, ok)
	}

	// Fresh contributor connection (the baseline one holds nothing: the
	// offer above is not an assignment until accepted) — #353 is withheld.
	msg := hub.selectTask(claimedIssueConn())
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", msg)
	}
	if msg.Number != 353 && msg.Number != 360 {
		t.Fatalf("unexpected issue #%d", msg.Number)
	}
	if msg.Number == 353 {
		t.Fatal("#353 offered after the webhook recorded an open PR claiming it")
	}

	// The PR closes without merging: the claim is released and #353 is
	// offerable again.
	closed := []byte(`{"action":"closed","repository":{"full_name":"projectbluefin/dakota"},"pull_request":{"number":900,"state":"closed","title":"fix it","body":"Fixes #353","html_url":"https://github.com/projectbluefin/dakota/pull/900","user":{"login":"castrojo"},"head":{"ref":"fix-353","sha":"abc"}}}`)
	postSignedWebhook(t, s, "pull_request", closed)
	if _, ok := ledger.Lookup("dakota", 353); ok {
		t.Fatal("claim must be released when the PR closes unmerged")
	}
	if msg := hub.selectTask(claimedIssueConn()); msg == nil || msg.Number != 353 {
		t.Fatalf("offer after PR closed = %+v, want #353 back", msg)
	}
}

func TestWebhookIssueClosedStopsSelectTaskOffering(t *testing.T) {
	hub, s, _ := webhookClaimFixture(t)
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	t.Cleanup(ghpkg.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))

	body := []byte(`{"action":"closed","issue":{"number":353,"state":"closed","title":"the issue everyone kept fixing"},"repository":{"full_name":"projectbluefin/dakota"}}`)
	postSignedWebhook(t, s, "issues", body)

	s.statusMu.RLock()
	remaining := len(s.status.Repos[0].ActionableIssues)
	s.statusMu.RUnlock()
	if remaining != 1 {
		t.Fatalf("actionable issues after close = %d, want 1 (#353 evicted)", remaining)
	}
	msg := hub.selectTask(claimedIssueConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 360 {
		t.Fatalf("offer after #353 closed = %+v, want #360", msg)
	}
}
