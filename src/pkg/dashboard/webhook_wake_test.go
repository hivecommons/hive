package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// TestGitHubWebhookReceiverWakesGovernorOnInvalidation: a verified delivery
// that invalidates cached state asks the governor loop for an early cycle
// (through Dependencies.WebhookWake) and names the reason; a delivery that
// changes nothing does not wake.
func TestGitHubWebhookReceiverWakesGovernorOnInvalidation(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	t.Cleanup(github.ResetWebhookTrackerForTest())
	s := newGitHubWebhookTestServer(t)
	var wakes []string
	s.deps.WebhookWake = func(reason string) { wakes = append(wakes, reason) }

	ping := []byte(`{"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if rec := postGitHubWebhook(s, "ping", signGitHubWebhookForTest(secret, ping), ping); rec.Code != http.StatusOK {
		t.Fatalf("ping: %d %s", rec.Code, rec.Body.String())
	}
	if len(wakes) != 0 {
		t.Fatalf("a delivery that invalidates nothing must not wake: %v", wakes)
	}

	review := []byte(`{"action":"submitted","pull_request":{"number":12},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	rec := postGitHubWebhook(s, "pull_request_review", signGitHubWebhookForTest(secret, review), review)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body.String())
	}
	if len(wakes) != 1 || wakes[0] != "pull_request_review webhookorg/webhookrepo #12" {
		t.Fatalf("wakes = %v, want one named reason", wakes)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["wake"] != "pull_request_review webhookorg/webhookrepo #12" {
		t.Fatalf("response should report the wake: %v", resp)
	}

	// Without the hook the receiver still works and simply does not wake.
	s.deps.WebhookWake = nil
	if rec := postGitHubWebhook(s, "pull_request_review", signGitHubWebhookForTest(secret, review), review); rec.Code != http.StatusOK {
		t.Fatalf("no hook: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookWakeReasonNamesItems(t *testing.T) {
	if _, ok := webhookWakeReason("ping", github.WebhookEventResult{KnownRepo: true, Repo: "r"}); ok {
		t.Fatal("nothing invalidated → no wake")
	}
	if _, ok := webhookWakeReason("pull_request", github.WebhookEventResult{Repo: "r", Invalidated: []int{1}}); ok {
		t.Fatal("unknown repo → no wake")
	}
	got, ok := webhookWakeReason("push", github.WebhookEventResult{KnownRepo: true, Repo: "r", Invalidated: []int{1, 2, 3, 4, 5}})
	if !ok || got != "push r #1,#2,#3,+2" {
		t.Fatalf("reason = %q", got)
	}
	got, ok = webhookWakeReason("issues", github.WebhookEventResult{KnownRepo: true, Repo: "r", DirtyIssues: []int{9}, ReopenedIssue: 4})
	if !ok || got != "issues r issue #9,reopened issue #4" {
		t.Fatalf("reason = %q", got)
	}
}

func TestGitHubIssueOpenedDeliveryWakesExistingEvaluation(t *testing.T) {
	const secret = "issue-opened-fixture"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	t.Cleanup(github.ResetWebhookTrackerForTest())
	s := newGitHubWebhookTestServer(t)
	var wakes []string
	s.deps.WebhookWake = func(reason string) { wakes = append(wakes, reason) }
	body := []byte(`{"action":"opened","issue":{"number":23,"state":"open"},"repository":{"full_name":"webhookorg/webhookrepo"}}`)

	rec := postGitHubWebhook(s, "issues", "invalid-signature", body)
	if rec.Code != http.StatusUnauthorized || len(wakes) != 0 {
		t.Fatalf("unverified delivery woke evaluation: status=%d wakes=%v", rec.Code, wakes)
	}
	rec = postGitHubWebhook(s, "issues", signGitHubWebhookForTest(secret, body), body)
	if rec.Code != http.StatusOK || len(wakes) != 1 || wakes[0] != "issues webhookorg/webhookrepo issue #23" {
		t.Fatalf("verified issue opened: status=%d wakes=%v body=%s", rec.Code, wakes, rec.Body.String())
	}
}

func TestStatusReportsEvalLoopPacing(t *testing.T) {
	s := newTestServer()
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s.deps = &Dependencies{EvalLoop: func() EvalLoopStatus {
		return EvalLoopStatus{Mode: "webhook", IntervalS: 900, LastWakeReason: "issue_comment r #3", LastWakeAt: &at, WebhookWakes: 2}
	}}
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "loop-test"}
	s.statusMu.Unlock()

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["evalMode"] != "webhook" || got["lastWakeReason"] != "issue_comment r #3" || got["evalIntervalS"] != float64(900) || got["webhookWakes"] != float64(2) {
		t.Fatalf("status = %v", got)
	}
	if got["lastWakeAt"] != "2026-10-20T12:00:00Z" || got["hiveId"] != "loop-test" {
		t.Fatalf("status = %v", got)
	}

	// Without the hook the keys are absent rather than defaulted.
	s.deps.EvalLoop = nil
	rec = httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if strings.Contains(rec.Body.String(), `"evalMode"`) {
		t.Fatalf("evalMode should be omitted without the loop hook: %s", rec.Body.String())
	}
}

// TestUpdateStatusInvalidatesStageTrackerAndNudgesSSE: a status publish drops
// the memoised /api/stage-tracker body (so the next GET rebuilds, not
// time-based only) and sends a `stage-tracker` SSE event so the SPA refetches
// now instead of on its poll.
func TestUpdateStatusInvalidatesStageTrackerAndNudgesSSE(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s, _ := stageTrackerFixture(t, now)
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now); !fresh {
		t.Fatal("first build should be fresh")
	}
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now.Add(time.Second)); fresh {
		t.Fatal("inside the TTL with no publish the cache must serve")
	}

	ch := make(chan []byte, 4)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	s.UpdateStatus(minimalPayload())

	if _, _, _, fresh := s.stageTrackerPayload(s.status, now.Add(2*time.Second)); !fresh {
		t.Fatal("a status publish must invalidate the stage tracker cache")
	}
	var frames []string
	for len(frames) < 2 {
		select {
		case f := <-ch:
			frames = append(frames, string(f))
		case <-time.After(time.Second):
			t.Fatalf("frames so far: %q", frames)
		}
	}
	if !strings.HasPrefix(frames[0], "data: ") {
		t.Fatalf("first frame should be the status payload: %q", frames[0])
	}
	if !strings.HasPrefix(frames[1], "event: stage-tracker\ndata: {\"seq\":") {
		t.Fatalf("second frame should nudge the stage tracker: %q", frames[1])
	}
}

func TestIndexHTMLRefetchesStageTrackerOnSSENudge(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`source.addEventListener('stage-tracker', () => {`,
		`if (_stageTrackerSig || _stageTrackerEtag) stageTrackerNudge();`,
		`const wait = _stageTrackerNudgeAt + STAGE_TRACKER_NUDGE_COALESCE_MS - now;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
