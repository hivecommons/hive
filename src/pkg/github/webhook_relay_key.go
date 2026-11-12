package github

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Hub webhook relay key (hivecommons/hive#17911).
//
// A hosted spoke never holds the GitHub App webhook secret, so it cannot
// verify X-Hub-Signature-256 on a delivery the hub relays. The hub instead
// signs each relay with a key of this hive's own, delivered over the
// heartbeat (HeartbeatGitHubAppConfig.WebhookRelayKey); the receiver
// (pkg/dashboard api_github_webhook.go) verifies that signature. The key is
// persisted 0600 on the data volume so a restarted spoke accepts relays
// before its first heartbeat, and is only ever reported by fingerprint.

const defaultWebhookRelayKeyPath = "/data/webhook-relay.key"

// Rejection reasons for hub-relayed deliveries, recorded through
// RecordWebhookDeliveryFailure and reported as webhooks.mode_reason_code
// alongside the GitHub-signature reasons.
const (
	// WebhookReasonRelaySignatureFailed: the hub relay signature did not
	// verify against this hive's relay key.
	WebhookReasonRelaySignatureFailed = "relay_signature_invalid"
	// WebhookReasonRelayDeliveryMissing: a signed relay carried no
	// X-GitHub-Delivery to bind replay protection to.
	WebhookReasonRelayDeliveryMissing = "relay_delivery_missing"
	// WebhookReasonRelayStale: the relay timestamp is outside the skew window.
	WebhookReasonRelayStale = "relay_timestamp_stale"
	// WebhookReasonRelayReplayed: the delivery ID was already accepted.
	WebhookReasonRelayReplayed = "relay_delivery_replayed"
)

// Values of /api/status.webhooks.verification: how this spoke authenticates
// GitHub webhook deliveries.
const (
	// WebhookVerificationGitHub: GITHUB_WEBHOOK_SECRET is set and GitHub's
	// X-Hub-Signature-256 is verified (self-hosted, or a hub relay to a spoke
	// that holds the secret).
	WebhookVerificationGitHub = "github-signed"
	// WebhookVerificationRelay: no secret, but the hub delivered this hive's
	// relay key, so hub-relayed deliveries are verified by the hub's signature.
	WebhookVerificationRelay = "relay (hub-signed)"
	// WebhookVerificationNone: neither; every delivery is rejected with 503.
	WebhookVerificationNone = "none"
)

var webhookRelayKeyState = struct {
	mu     sync.Mutex
	path   string
	key    string
	loaded bool
}{path: defaultWebhookRelayKeyPath}

func webhookRelayKeyLocked() string {
	st := &webhookRelayKeyState
	if !st.loaded {
		st.loaded = true
		if data, err := os.ReadFile(st.path); err == nil {
			st.key = strings.TrimSpace(string(data))
		}
	}
	return st.key
}

// WebhookRelayKey is the hub relay key this spoke holds, or "".
func WebhookRelayKey() string {
	webhookRelayKeyState.mu.Lock()
	defer webhookRelayKeyState.mu.Unlock()
	return webhookRelayKeyLocked()
}

// AdoptWebhookRelayKey stores a relay key delivered by the hub and reports
// whether it replaced the one held (a first delivery or a rotation).
//
// An empty key means "the hub is not speaking to this field", never "clear
// it", so a beat without the key can never blank a working one. A key that
// cannot be persisted is still adopted in memory (relays verify until the
// next restart) and the write error is returned for the caller to log.
func AdoptWebhookRelayKey(key string) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return false, nil
	}
	st := &webhookRelayKeyState
	st.mu.Lock()
	defer st.mu.Unlock()
	if webhookRelayKeyLocked() == key {
		return false, nil
	}
	st.key = key
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return true, err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0o600); err != nil {
		return true, err
	}
	return true, os.Rename(tmp, st.path)
}

// WebhookRelayKeyFingerprint is the NON-SECRET "sha256:<hex>" of the relay key
// this spoke holds, reported on every heartbeat so the hub delivers the key
// only when the spoke lacks it or holds a rotated-out one. Empty when none.
func WebhookRelayKeyFingerprint() string {
	key := WebhookRelayKey()
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// WebhookVerification reports how this spoke verifies webhook deliveries
// (WebhookVerification* above). Only the mode ever leaves this function, never
// the secret or the key.
func WebhookVerification() string {
	if WebhookSecretConfigured() {
		return WebhookVerificationGitHub
	}
	if WebhookRelayKey() != "" {
		return WebhookVerificationRelay
	}
	return WebhookVerificationNone
}

// SetWebhookRelayKeyPathForTest points the relay key store at path, forgets
// any key held in memory, and returns a func that restores the previous state.
func SetWebhookRelayKeyPathForTest(path string) func() {
	st := &webhookRelayKeyState
	st.mu.Lock()
	prevPath, prevKey, prevLoaded := st.path, st.key, st.loaded
	st.path, st.key, st.loaded = path, "", false
	st.mu.Unlock()
	return func() {
		st.mu.Lock()
		st.path, st.key, st.loaded = prevPath, prevKey, prevLoaded
		st.mu.Unlock()
	}
}
