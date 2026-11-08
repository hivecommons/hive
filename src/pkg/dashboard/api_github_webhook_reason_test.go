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

// readStatusWebhooks returns /api/status?fields=webhooks as a map.
func readStatusWebhooks(t *testing.T, s *Server) map[string]any {
	t.Helper()
	s.statusMu.Lock()
	if s.status == nil {
		s.status = &StatusPayload{HiveID: "webhook-reason-test"}
	}
	s.statusMu.Unlock()
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status?fields=webhooks", nil))
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	webhooks, ok := status["webhooks"].(map[string]any)
	if !ok {
		t.Fatalf("/api/status has no webhooks block: %s", rec.Body.String())
	}
	return webhooks
}

// readDeepWebhookCheck returns the body of GET /api/health/deep and its
// `webhooks` check.
func readDeepWebhookCheck(t *testing.T, s *Server) (string, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleHealthDeep(rec, httptest.NewRequest(http.MethodGet, "/api/health/deep", nil))
	var out struct {
		Status string         `json:"status"`
		Checks map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	check, ok := out.Checks[healthCheckWebhooks].(map[string]any)
	if !ok {
		t.Fatalf("/api/health/deep has no %q check: %s", healthCheckWebhooks, rec.Body.String())
	}
	return rec.Body.String(), check
}

// summaryWebhookCheck returns the webhooks entry of the heartbeat/dashboard
// health summary (the navbar health tooltip reads it).
func summaryWebhookCheck(t *testing.T, s *Server) (status, detail string) {
	t.Helper()
	raw, err := json.Marshal(s.HealthSummary())
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Checks {
		if c.Name == healthCheckWebhooks {
			return c.Status, c.Detail
		}
	}
	t.Fatalf("health summary has no %q check: %s", healthCheckWebhooks, raw)
	return "", ""
}

// A spoke with no GITHUB_WEBHOOK_SECRET answers every relayed delivery 503.
// That used to leave /api/status at `healthy:false, mode:polling` with no
// reason, the same as "GitHub is not sending". The status now names the
// cause and counts the rejections, while the log stays at one WARN.
func TestGitHubWebhookNoSecretReportsReasonAndCountsRejections(t *testing.T) {
	t.Cleanup(github.ResetWebhookTrackerForTest())
	t.Setenv(githubWebhookSecretEnvVar, "")
	s := newGitHubWebhookTestServer(t)
	body := []byte(`{"number":5,"repository":{"full_name":"webhookorg/webhookrepo"}}`)

	for i := 0; i < 3; i++ {
		if rec := postGitHubWebhook(s, "pull_request", "sha256=00", body); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: status = %d, want 503", i, rec.Code)
		}
	}
	got := readStatusWebhooks(t, s)
	if got["mode"] != string(github.WebhookModePolling) || got["mode_reason_code"] != github.WebhookReasonNoSecret {
		t.Fatalf("webhooks = %v, want polling with mode_reason_code %q", got, github.WebhookReasonNoSecret)
	}
	if reason, _ := got["mode_reason"].(string); !strings.Contains(reason, "no GITHUB_WEBHOOK_SECRET") || !strings.Contains(reason, "503") {
		t.Fatalf("mode_reason = %q, want the plain-words no-secret cause", reason)
	}
	if n, _ := got["rejected_1h"].(float64); n != 3 {
		t.Fatalf("rejected_1h = %v, want 3", got["rejected_1h"])
	}
	if at, _ := got["last_rejected_at"].(string); at == "" {
		t.Fatalf("last_rejected_at = %v, want the time of the last 503", got["last_rejected_at"])
	} else if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
		t.Fatalf("last_rejected_at = %q: %v", at, err)
	}
}

// The deep health check (health_deep_read) and the dashboard health summary
// carry the no-secret cause as a warning that never changes the verdict.
func TestHealthDeepWarnsWhenWebhooksRejectedForMissingSecret(t *testing.T) {
	t.Cleanup(github.ResetWebhookTrackerForTest())
	t.Setenv(githubWebhookSecretEnvVar, "")
	s := newGitHubWebhookTestServer(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "webhook-health-test"}
	s.ready = true
	s.statusMu.Unlock()

	// Not configured and nobody sending: polling by design, not a warning.
	if _, check := readDeepWebhookCheck(t, s); check["status"] != "pass" || check["mode_reason_code"] != github.WebhookReasonNoSecret {
		t.Fatalf("before any delivery webhooks check = %v, want pass with no_secret", check)
	}

	if rec := postGitHubWebhook(s, "pull_request", "", []byte(`{}`)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body, check := readDeepWebhookCheck(t, s)
	detail, _ := check["detail"].(string)
	if check["status"] != "warn" || check["mode_reason_code"] != github.WebhookReasonNoSecret ||
		!strings.Contains(detail, "no GITHUB_WEBHOOK_SECRET on this spoke") || !strings.Contains(detail, "rejected with 503") {
		t.Fatalf("webhooks check = %v, want a no-secret warning", check)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("a webhook warning must not change the overall verdict: %s", body)
	}
	if st, d := summaryWebhookCheck(t, s); st != "warn" || !strings.Contains(d, "no GITHUB_WEBHOOK_SECRET") {
		t.Fatalf("health summary webhooks = %s %q, want the same warning", st, d)
	}
	// The summary feeds the header pill and DeepHealth: the warning is listed
	// there but does not move its verdict either (#17173 review).
	if sum := s.HealthSummary(); sum["warns"] != 0 || sum["status"] != "ok" {
		t.Fatalf("health summary verdict = %v (warns %v), want ok with the webhook check not counted", sum["status"], sum["warns"])
	}
}

// One rejection outside the hourly window must not keep the spoke at warn
// for good: the no-secret warning follows Rejected1h, not LastRejectedAt
// (#17173 review).
func TestWebhookHealthNoSecretWarnsOnlyWithinTheHour(t *testing.T) {
	old := time.Now().Add(-26 * time.Hour)
	snap := github.WebhookHealth{WebhookModeStatus: github.WebhookModeStatus{Mode: github.WebhookModePolling, ReasonCode: github.WebhookReasonNoSecret}, LastRejectedAt: &old}
	if st, d := webhookHealth(snap); st != "pass" {
		t.Fatalf("stale rejection: webhookHealth = %s %q, want pass", st, d)
	}
	snap.Rejected1h = 2
	if st, _ := webhookHealth(snap); st != "warn" {
		t.Fatalf("rejections in the last hour: webhookHealth = %s, want warn", st)
	}
}

// With the secret set, the cause is "no deliveries" until a verified delivery
// arrives; then it clears and the check passes. The secret itself never
// appears in the status or health output.
func TestWebhookReasonClearsAfterVerifiedDelivery(t *testing.T) {
	const secret = "s3cr3t-never-shown"
	t.Cleanup(github.ResetWebhookTrackerForTest())
	t.Setenv(githubWebhookSecretEnvVar, secret)
	s := newGitHubWebhookTestServer(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "webhook-clear-test"}
	s.ready = true
	s.statusMu.Unlock()

	if got := readStatusWebhooks(t, s); got["mode_reason_code"] != github.WebhookReasonNoDeliveries {
		t.Fatalf("before any delivery webhooks = %v, want mode_reason_code %q", got, github.WebhookReasonNoDeliveries)
	}

	known := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if rec := postGitHubWebhook(s, "pull_request", "sha256=00", known); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status = %d, want 401", rec.Code)
	}
	if got := readStatusWebhooks(t, s); got["mode_reason_code"] != github.WebhookReasonSignatureFailed {
		t.Fatalf("after a bad signature webhooks = %v, want mode_reason_code %q", got, github.WebhookReasonSignatureFailed)
	}
	if _, check := readDeepWebhookCheck(t, s); check["status"] != "warn" {
		t.Fatalf("after a bad signature webhooks check = %v, want warn", check)
	}

	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(secret, known), known); rec.Code != http.StatusOK {
		t.Fatalf("delivery: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := readStatusWebhooks(t, s); got["mode_reason_code"] != nil || got["healthy"] != true {
		t.Fatalf("after a verified delivery webhooks = %v, want healthy with no reason code", got)
	}
	github.ObserveWebhookMode()
	got := readStatusWebhooks(t, s)
	if got["mode"] != string(github.WebhookModeWebhook) || got["mode_reason_code"] != nil {
		t.Fatalf("after observe webhooks = %v, want webhook mode with no reason", got)
	}
	body, check := readDeepWebhookCheck(t, s)
	if check["status"] != "pass" {
		t.Fatalf("webhooks check = %v, want pass", check)
	}
	raw, _ := json.Marshal(got)
	summary, _ := json.Marshal(s.HealthSummary())
	for _, out := range []string{string(raw), body, string(summary)} {
		if strings.Contains(out, secret) {
			t.Fatalf("webhook secret leaked into output: %s", out)
		}
	}
}
