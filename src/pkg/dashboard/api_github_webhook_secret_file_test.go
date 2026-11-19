package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// TestGitHubWebhookReceiverAcceptsFileSecret: with no GITHUB_WEBHOOK_SECRET,
// the receiver verifies deliveries against the secret file the owner wrote
// (/data/github-webhook-secret.key in production), with no restart; the env
// var still wins when both are present (hivecommons/hive#18025).
func TestGitHubWebhookReceiverAcceptsFileSecret(t *testing.T) {
	const fileSecret = "from-the-file"
	t.Setenv(githubWebhookSecretEnvVar, "")
	path := filepath.Join(t.TempDir(), "github-webhook-secret.key")
	t.Cleanup(github.SetWebhookSecretPathForTest(path))
	t.Cleanup(github.SetWebhookRelayKeyPathForTest(filepath.Join(t.TempDir(), "relay.key")))
	s := newGitHubWebhookTestServer(t)
	body := []byte(`{"number":5,"repository":{"full_name":"someone/else"}}`)

	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(fileSecret, body), body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no env, no file: status = %d, want 503", rec.Code)
	}

	if err := os.WriteFile(path, []byte(fileSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The receiver read (and cached) "no secret" above; a fresh store load
	// stands in for the dashboard writing the file through the package.
	t.Cleanup(github.SetWebhookSecretPathForTest(path))

	if got := github.WebhookSecretSource(); got != github.WebhookSecretSourceFile {
		t.Fatalf("source = %q, want file", got)
	}
	if got := github.WebhookVerification(); got != github.WebhookVerificationGitHub {
		t.Fatalf("verification = %q, want %q", got, github.WebhookVerificationGitHub)
	}
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(fileSecret, body), body); rec.Code != http.StatusOK {
		t.Fatalf("file secret, correctly signed: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("wrong", body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("file secret, wrong signature: status = %d, want 401", rec.Code)
	}

	t.Setenv(githubWebhookSecretEnvVar, "from-the-env")
	if got := github.WebhookSecretSource(); got != github.WebhookSecretSourceEnv {
		t.Fatalf("source with env set = %q, want env", got)
	}
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(fileSecret, body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("env set, signed with file secret: status = %d, want 401", rec.Code)
	}
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("from-the-env", body), body); rec.Code != http.StatusOK {
		t.Fatalf("env set, signed with env secret: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}
