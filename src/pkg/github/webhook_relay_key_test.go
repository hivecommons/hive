package github

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestAdoptWebhookRelayKey: the hub relay key delivered over the heartbeat is
// persisted 0600 and survives a restart, a rotation replaces it, and a beat
// that carries no key never blanks a working one (#17911).
func TestAdoptWebhookRelayKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhook-relay.key")
	t.Cleanup(SetWebhookRelayKeyPathForTest(path))
	t.Setenv(WebhookSecretEnvVar, "")

	if WebhookRelayKey() != "" || WebhookRelayKeyFingerprint() != "" || WebhookVerification() != WebhookVerificationNone {
		t.Fatal("fresh spoke must hold no relay key")
	}

	changed, err := AdoptWebhookRelayKey("key-1")
	if err != nil || !changed {
		t.Fatalf("first delivery: changed=%v err=%v, want adopted", changed, err)
	}
	if WebhookRelayKey() != "key-1" || WebhookVerification() != WebhookVerificationRelay {
		t.Fatalf("after delivery: key=%q verification=%q", WebhookRelayKey(), WebhookVerification())
	}
	fp := WebhookRelayKeyFingerprint()
	// Same format the hub compares against (pkg/hub webhookRelayKeyFingerprint).
	sum := sha256.Sum256([]byte("key-1"))
	if want := "sha256:" + hex.EncodeToString(sum[:]); fp != want {
		t.Fatalf("fingerprint = %q, want %q", fp, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("relay key file: %v, want mode 0600", err)
	}

	if changed, err := AdoptWebhookRelayKey("key-1"); err != nil || changed {
		t.Fatalf("redelivery of the same key: changed=%v err=%v, want no change", changed, err)
	}
	for _, empty := range []string{"", "  "} {
		if changed, err := AdoptWebhookRelayKey(empty); err != nil || changed {
			t.Fatalf("empty delivery %q: changed=%v err=%v", empty, changed, err)
		}
		if WebhookRelayKey() != "key-1" {
			t.Fatal("an empty delivery blanked a working relay key")
		}
	}

	if changed, err := AdoptWebhookRelayKey("key-2"); err != nil || !changed {
		t.Fatalf("rotation: changed=%v err=%v, want adopted", changed, err)
	}
	if WebhookRelayKeyFingerprint() == fp {
		t.Fatal("fingerprint did not change after rotation")
	}

	// A restarted spoke reads the persisted key before its first heartbeat.
	restore := SetWebhookRelayKeyPathForTest(path)
	defer restore()
	if WebhookRelayKey() != "key-2" {
		t.Fatalf("after restart key = %q, want the rotated key from disk", WebhookRelayKey())
	}

	t.Setenv(WebhookSecretEnvVar, "s3cr3t")
	if WebhookVerification() != WebhookVerificationGitHub {
		t.Fatalf("with GITHUB_WEBHOOK_SECRET set verification = %q, want %q", WebhookVerification(), WebhookVerificationGitHub)
	}
}
