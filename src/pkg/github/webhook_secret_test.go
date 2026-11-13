package github

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebhookSecret_FileFallbackAndGenerate(t *testing.T) {
	t.Setenv(WebhookSecretEnvVar, "")
	dir := t.TempDir()
	restore := SetWebhookSecretPathForTest(filepath.Join(dir, "nested", "secret.key"))
	defer restore()

	if got := WebhookSecret(); got != "" {
		t.Fatalf("no file yet: WebhookSecret() = %q, want empty", got)
	}
	if got := WebhookSecretSource(); got != WebhookSecretSourceNone {
		t.Fatalf("source = %q, want none", got)
	}
	if got := WebhookVerification(); got == WebhookVerificationGitHub {
		t.Fatalf("verification = %q before any secret", got)
	}

	secret, err := GenerateWebhookSecret()
	if err != nil {
		t.Fatalf("GenerateWebhookSecret: %v", err)
	}
	if len(secret) != webhookSecretBytes*2 {
		t.Fatalf("secret length %d, want %d hex chars", len(secret), webhookSecretBytes*2)
	}
	if got := WebhookSecret(); got != secret {
		t.Fatalf("WebhookSecret() = %q, want the generated value", got)
	}
	if got := WebhookSecretSource(); got != WebhookSecretSourceFile {
		t.Fatalf("source = %q, want file", got)
	}
	if got := WebhookVerification(); got != WebhookVerificationGitHub {
		t.Fatalf("verification = %q, want %q", got, WebhookVerificationGitHub)
	}
	st, err := os.Stat(WebhookSecretPath())
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secret file mode %o, want 0600", perm)
	}

	// A fresh process reads the persisted value back.
	restore2 := SetWebhookSecretPathForTest(WebhookSecretPath())
	defer restore2()
	if got := WebhookSecret(); got != secret {
		t.Fatalf("reloaded secret = %q, want %q", got, secret)
	}

	// Regenerating replaces it.
	next, err := GenerateWebhookSecret()
	if err != nil || next == secret {
		t.Fatalf("regenerate: err=%v same=%v", err, next == secret)
	}
}

func TestWebhookSecret_EnvWinsAndBlocksGenerate(t *testing.T) {
	restore := SetWebhookSecretPathForTest(filepath.Join(t.TempDir(), "secret.key"))
	defer restore()
	if _, err := GenerateWebhookSecret(); err != nil {
		t.Fatalf("seed file secret: %v", err)
	}
	t.Setenv(WebhookSecretEnvVar, "from-env")
	if got := WebhookSecret(); got != "from-env" {
		t.Fatalf("WebhookSecret() = %q, want env value", got)
	}
	if got := WebhookSecretSource(); got != WebhookSecretSourceEnv {
		t.Fatalf("source = %q, want env", got)
	}
	if _, err := GenerateWebhookSecret(); err != ErrWebhookSecretFromEnv {
		t.Fatalf("GenerateWebhookSecret with env set: err=%v, want ErrWebhookSecretFromEnv", err)
	}
}

func TestWebhookHookPermissions(t *testing.T) {
	org, repo := WebhookHookPermissions(nil)
	if org || repo {
		t.Fatal("nil info must grant nothing")
	}
	info := &InstallationInfo{Permissions: map[string]string{PermissionOrganizationHooks: "write", PermissionRepositoryHooks: "read"}}
	org, repo = WebhookHookPermissions(info)
	if !org || repo {
		t.Fatalf("org=%v repo=%v, want org write only", org, repo)
	}
}

func TestWebBaseURLForAPI(t *testing.T) {
	cases := map[string]string{
		"":                                "https://github.com",
		"https://api.github.com":          "https://github.com",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com",
	}
	for in, want := range cases {
		if got := webBaseURLForAPI(in); got != want {
			t.Errorf("webBaseURLForAPI(%q) = %q, want %q", in, got, want)
		}
	}
}
