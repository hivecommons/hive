package dashboard

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func signedWebhookRequest(body []byte, secret, sigSecret string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	mac := hmac.New(sha256.New, []byte(sigSecret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestGitHubWebhookSignatureFailureRejected(t *testing.T) {
	t.Setenv(githubWebhookSecretEnvVar, "secret")
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{GHClient: ghpkg.NewClientForTest("http://127.0.0.1:1", "org", []string{"repo"}, slog.Default())}
	body := []byte(`{"repository":{"full_name":"org/repo"},"pull_request":{"number":7}}`)
	rec := httptest.NewRecorder()
	s.handleGitHubWebhookInvalidation(rec, signedWebhookRequest(body, "secret", "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookStatusFieldsPresent(t *testing.T) {
	got := webhookHealthMap(ghpkg.WebhookHealthSnapshot{Healthy: true, Events1h: 2, Invalidations1h: 1, IntervalS: 900})
	for _, k := range []string{"healthy", "events_1h", "invalidations_1h", "interval_s"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing %s in %+v", k, got)
		}
	}
	if !strings.Contains(debugFieldNamesForWebhookTest(got), "healthy") {
		t.Fatal(got)
	}
}

func debugFieldNamesForWebhookTest(m map[string]any) string {
	var b strings.Builder
	for k := range m {
		b.WriteString(k)
	}
	return b.String()
}
