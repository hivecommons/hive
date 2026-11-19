package hub

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// Hub-signed webhook relay (hivecommons/hive#17911).
//
// A hosted spoke must be able to verify a delivery the hub relays to it, but
// it must never hold the GitHub App webhook secret: that one secret
// authenticates GitHub deliveries for the whole App, so any spoke holding it
// could forge deliveries to every other spoke's public /api/webhook/github.
// Instead every hosted hive gets its OWN random relay key, stored on its hub
// record (SaaSHive.WebhookRelayKey) and delivered over its per-hive
// authenticated heartbeat. The hub signs each relay with the target hive's
// key; a key held by one spoke cannot sign anything another spoke accepts.
//
// Signature scheme (mirrored by pkg/dashboard verifyWebhookRelaySignature):
//
//	X-Hive-Relay-Timestamp:     <unix seconds at relay time>
//	X-Hive-Relay-Signature-256: sha256=<hex HMAC-SHA256(key, timestamp + "." + X-GitHub-Delivery + "." + raw body)>
//
// The timestamp and delivery ID are inside the MAC so the spoke can reject
// stale relays and replayed delivery IDs without trusting unsigned headers.
// GitHub's own headers (X-Hub-Signature-256 included) are forwarded
// unchanged next to these, so a self-hosted spoke is unaffected.
const (
	webhookRelaySignatureHeader = "X-Hive-Relay-Signature-256"
	webhookRelayTimestampHeader = "X-Hive-Relay-Timestamp"
)

// newWebhookRelayKey mints a 256-bit relay key, hex encoded.
func newWebhookRelayKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// webhookRelayKeyFingerprint is the NON-SECRET identifier of a relay key the
// spoke reports back on every heartbeat (webhook_relay_key_fingerprint), so the
// hub delivers the key only when the spoke does not already hold it. Same
// format as pkg/github WebhookRelayKeyFingerprint.
func webhookRelayKeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// webhookRelaySignature computes the relay MAC described above.
func webhookRelaySignature(key, timestamp, delivery string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(delivery))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// signWebhookRelayHeaders adds the relay timestamp and signature for one
// target hive's key to an outgoing relay.
func signWebhookRelayHeaders(h http.Header, key string, now time.Time, delivery string, body []byte) {
	ts := strconv.FormatInt(now.Unix(), 10)
	h.Set(webhookRelayTimestampHeader, ts)
	h.Set(webhookRelaySignatureHeader, webhookRelaySignature(key, ts, delivery, body))
}

// webhookRelayKeyForHive is the relay key on a hive's hub record, or "" for a
// hive the hub did not provision (a self-hosted spoke keeps verifying GitHub's
// signature with its own secret).
func webhookRelayKeyForHive(hiveID string) string {
	if h := loadSaaSHive(hiveID); h != nil {
		return h.WebhookRelayKey
	}
	return ""
}

// webhookRelayKeyForHeartbeat returns the relay key to deliver on this beat,
// or "" when the spoke already reports holding it.
//
// A hosted hive provisioned before relay keys existed has none on its record;
// one is minted here on its first beat so the fix reaches the existing fleet.
// Rotation is the same path: replace (or clear) SaaSHive.WebhookRelayKey and
// the spoke's fingerprint stops matching, so the next beat delivers the new
// key. The beat is authenticated with the per-hive bearer before this runs
// (verifyHeartbeatBearer), so a spoke can only ever be handed its own key.
func (s *HubServer) webhookRelayKeyForHeartbeat(p *HeartbeatPayload) string {
	if p == nil || p.HiveID == "" {
		return ""
	}
	h := loadSaaSHive(p.HiveID)
	if h == nil {
		return ""
	}
	if h.WebhookRelayKey == "" {
		key := newWebhookRelayKey()
		if key == "" {
			return ""
		}
		h.WebhookRelayKey = key
		if err := saveSaaSHive(h); err != nil {
			s.logger.Warn("webhook relay: could not store a new relay key on the hive record", "hive_id", p.HiveID, "error", err)
			return ""
		}
		s.logger.Info("webhook relay: minted relay key for hive", "hive_id", p.HiveID, "fingerprint", webhookRelayKeyFingerprint(key))
	}
	if p.WebhookRelayKeyFingerprint == webhookRelayKeyFingerprint(h.WebhookRelayKey) {
		return ""
	}
	return h.WebhookRelayKey
}
