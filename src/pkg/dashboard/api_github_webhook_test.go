package dashboard

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func signGitHubWebhookForTest(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newGitHubWebhookTestServer(t *testing.T) *Server {
	t.Helper()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("webhook receiver must not call GitHub: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ghSrv.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.deps = &Dependencies{
		GHClient: github.NewClientForTest(ghSrv.URL, "webhookorg", []string{"webhookrepo"}, logger),
		Ctx:      context.Background(),
		Logger:   logger,
	}
	return s
}

func postGitHubWebhook(s *Server, event, signature string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, req)
	return rec
}

func TestGitHubWebhookReceiverFailsClosed(t *testing.T) {
	s := newGitHubWebhookTestServer(t)
	body := []byte(`{"number":5,"repository":{"full_name":"webhookorg/webhookrepo"}}`)

	t.Setenv(githubWebhookSecretEnvVar, "")
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("s3cr3t", body), body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret: status = %d, want 503", rec.Code)
	}

	t.Setenv(githubWebhookSecretEnvVar, "s3cr3t")
	if rec := postGitHubWebhook(s, "", signGitHubWebhookForTest("s3cr3t", body), body); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing event: status = %d, want 400", rec.Code)
	}
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("wrong", body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status = %d, want 401", rec.Code)
	}
	if rec := postGitHubWebhook(s, "pull_request", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: status = %d, want 401", rec.Code)
	}
	bad := []byte(`not json`)
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("s3cr3t", bad), bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed payload: status = %d, want 400", rec.Code)
	}
	if !isPublicPath(githubWebhookPath) {
		t.Fatal("GitHub cannot hold a dashboard session; the receiver path must be public")
	}
}

func TestGitHubWebhookReceiverInvalidatesKnownRepoAndIgnoresUnknown(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	s := newGitHubWebhookTestServer(t)

	unknown := []byte(`{"number":5,"repository":{"full_name":"someone/else"}}`)
	rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(secret, unknown), unknown)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown repo: status = %d, want 200", rec.Code)
	}
	var ignored map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ignored); err != nil || ignored["status"] != "ignored" {
		t.Fatalf("unknown repo response = %s, want status ignored", rec.Body.String())
	}

	known := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	rec = postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(secret, known), known)
	if rec.Code != http.StatusOK {
		t.Fatalf("known repo: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Status      string `json:"status"`
		Invalidated []int  `json:"invalidated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || !reflect.DeepEqual(got.Invalidated, []int{5}) {
		t.Fatalf("known repo response = %+v, want ok/[5]", got)
	}

	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "webhook-test"}
	s.statusMu.Unlock()
	statusRec := httptest.NewRecorder()
	s.handleStatus(statusRec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status map[string]any
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	webhooks, ok := status["webhooks"].(map[string]any)
	if !ok {
		t.Fatalf("/api/status has no webhooks block: %s", statusRec.Body.String())
	}
	for _, k := range []string{"healthy", "last_event_at", "events_1h", "invalidations_1h"} {
		if _, ok := webhooks[k]; !ok {
			t.Fatalf("/api/status webhooks missing %q: %v", k, webhooks)
		}
	}
	if webhooks["healthy"] != true || webhooks["events_1h"].(float64) < 1 {
		t.Fatalf("/api/status webhooks = %v, want healthy with at least one event", webhooks)
	}
	if status["hiveId"] != "webhook-test" {
		t.Fatalf("status payload fields must still be served alongside webhooks: %v", status["hiveId"])
	}

	filteredRec := httptest.NewRecorder()
	s.handleStatus(filteredRec, httptest.NewRequest(http.MethodGet, "/api/status?fields=webhooks", nil))
	var filtered map[string]any
	if err := json.Unmarshal(filteredRec.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if _, ok := filtered["webhooks"]; !ok || len(filtered) != 1 {
		t.Fatalf("?fields=webhooks = %v, want only webhooks", filtered)
	}
}

// TestStatusWebhooksModeAndSince: /api/status.webhooks carries the cadence
// mode (`mode` webhook|polling, `since`) the governor's reconcile tick
// follows (hivecommons/hive#13381), and deliveries that fail verification
// count as degradation once observed.
func TestStatusWebhooksModeAndSince(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	t.Cleanup(github.ResetWebhookTrackerForTest())
	s := newGitHubWebhookTestServer(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "webhook-mode-test"}
	s.statusMu.Unlock()

	readWebhooks := func() map[string]any {
		t.Helper()
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

	if got := readWebhooks(); got["mode"] != string(github.WebhookModePolling) || got["since"] == nil {
		t.Fatalf("before any delivery webhooks = %v, want mode polling with since", got)
	}

	known := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(secret, known), known); rec.Code != http.StatusOK {
		t.Fatalf("delivery: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, _, recovered := github.ObserveWebhookMode(); !recovered {
		t.Fatal("first verified delivery should engage webhook mode")
	}
	if got := readWebhooks(); got["mode"] != string(github.WebhookModeWebhook) || got["healthy"] != true {
		t.Fatalf("after a delivery webhooks = %v, want mode webhook", got)
	}

	// A delivery that fails signature verification is recorded as a failure
	// and, being newer than the last good one, degrades the cadence.
	if rec := postGitHubWebhook(s, "pull_request", "sha256=00", known); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status = %d, want 401", rec.Code)
	}
	if _, degraded, _ := github.ObserveWebhookMode(); !degraded {
		t.Fatal("a failing delivery after the last good one should degrade to polling")
	}
	got := readWebhooks()
	if got["mode"] != string(github.WebhookModePolling) || got["mode_reason"] != github.WebhookDegradedReasonFailures {
		t.Fatalf("after a failed delivery webhooks = %v, want mode polling with the failure reason", got)
	}
	if _, err := time.Parse(time.RFC3339Nano, got["since"].(string)); err != nil {
		t.Fatalf("since = %v, want RFC3339: %v", got["since"], err)
	}
}

func TestGitHubWebhookMissingSecretWarnsOnce(t *testing.T) {
	s := newGitHubWebhookTestServer(t)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Setenv(githubWebhookSecretEnvVar, "")
	body := []byte(`{}`)

	for i := 0; i < 3; i++ {
		if rec := postGitHubWebhook(s, "pull_request", "", body); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: status = %d, want 503", i, rec.Code)
		}
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Fatalf("WARN lines = %d, want 1; log:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), "level=DEBUG"); got != 2 {
		t.Fatalf("DEBUG lines = %d, want 2; log:\n%s", got, buf.String())
	}
}
