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

// A failed check_run delivery records one CI failure signal per PR, hands it
// to the governor hook once (a redelivery is deduplicated per head + check),
// and /api/status serves the count and the newest signal
// (hivecommons/hive#15291).
func TestGitHubWebhookFailedCheckRecordsCIFailureSignal(t *testing.T) {
	now := time.Date(2026, 10, 29, 4, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	s := newGitHubWebhookTestServer(t)
	s.status = &StatusPayload{HiveID: "webhook-test"}
	var got []github.CIFailureSignal
	s.deps.CIFailure = func(sig github.CIFailureSignal) { got = append(got, sig) }

	failed := []byte(`{"action":"completed","check_run":{"name":"unit","conclusion":"failure","head_sha":"abc123","details_url":"https://ci.example/run/7","pull_requests":[{"number":41}]},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	for i := 0; i < 2; i++ {
		rec := postGitHubWebhook(s, "check_run", signGitHubWebhookForTest(secret, failed), failed)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if want := i == 0; strings.Contains(rec.Body.String(), `"ci_failures":1`) != want {
			t.Fatalf("delivery %d: ci_failures reported = %v, want %v: %s", i, !want, want, rec.Body.String())
		}
	}
	if len(got) != 1 {
		t.Fatalf("hook got %d signals, want 1 (redelivery deduplicated): %+v", len(got), got)
	}
	sig := got[0]
	if sig.Repo != "webhookorg/webhookrepo" || sig.Number != 41 || sig.HeadSHA != "abc123" || sig.Check != "unit" || sig.DetailsURL != "https://ci.example/run/7" || !sig.At.Equal(now) {
		t.Fatalf("signal = %+v", sig)
	}

	passed := []byte(`{"action":"completed","check_run":{"name":"lint","conclusion":"success","head_sha":"abc123","pull_requests":[{"number":41}]},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if rec := postGitHubWebhook(s, "check_run", signGitHubWebhookForTest(secret, passed), passed); strings.Contains(rec.Body.String(), "ci_failures") {
		t.Fatalf("a passing check must not record a signal: %s", rec.Body.String())
	}
	status := []byte(`{"sha":"abc123","state":"error","context":"ci/legacy","target_url":"https://ci.example/legacy","repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if rec := postGitHubWebhook(s, "status", signGitHubWebhookForTest(secret, status), status); strings.Contains(rec.Body.String(), "ci_failures") {
		t.Fatalf("a status whose head no cached PR holds is left to the sweep: %s", rec.Body.String())
	}

	statusRec := httptest.NewRecorder()
	s.handleStatus(statusRec, httptest.NewRequest(http.MethodGet, "/api/status?fields=webhooks", nil))
	var payload struct {
		Webhooks struct {
			CIFailures github.CIFailureStatus `json:"ci_failures"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/status: %v: %s", err, statusRec.Body.String())
	}
	cf := payload.Webhooks.CIFailures
	if cf.Count != 1 || cf.Last == nil || cf.Last.Number != 41 || cf.Last.Check != "unit" || cf.Last.HeadSHA != "abc123" {
		t.Fatalf("/api/status webhooks.ci_failures = %+v, want count 1 and last #41 unit", cf)
	}
}
