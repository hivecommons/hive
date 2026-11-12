package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Hub relay signature on relayed GitHub webhooks (hivecommons/hive#17911).
//
// A hosted spoke never holds the GitHub App webhook secret, so the hub signs
// each delivery it relays with this hive's own relay key (delivered over the
// heartbeat; github.WebhookRelayKey). The scheme mirrors pkg/hub
// webhookRelaySignature:
//
//	X-Hive-Relay-Timestamp:     <unix seconds at relay time>
//	X-Hive-Relay-Signature-256: sha256=<hex HMAC-SHA256(key, timestamp + "." + X-GitHub-Delivery + "." + raw body)>
//
// After the MAC verifies, a timestamp more than webhookRelayMaxSkew from now
// is rejected as stale, and a delivery ID already accepted inside the replay
// window is rejected as replayed.
const (
	webhookRelaySignatureHeader = "X-Hive-Relay-Signature-256"
	webhookRelayTimestampHeader = "X-Hive-Relay-Timestamp"

	webhookRelayMaxSkew = 5 * time.Minute
	// webhookRelayReplayTTL covers every timestamp still inside the skew
	// window, so a captured relay cannot be replayed while it would verify.
	webhookRelayReplayTTL = 2 * webhookRelayMaxSkew
	// webhookRelayReplayMax bounds the remembered delivery IDs; the oldest is
	// dropped when a burst exceeds it.
	webhookRelayReplayMax = 4096
)

// verifyWebhookRelaySignature is the constant-time HMAC-SHA256 check of the
// relay signature, in the same style as verifyGitHubWebhookSignature.
func verifyWebhookRelaySignature(body []byte, timestamp, delivery, signature, key string) bool {
	if key == "" || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(delivery))
	mac.Write([]byte("."))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

// webhookRelayReplayCache remembers recently accepted relay delivery IDs.
// The zero value is ready to use.
type webhookRelayReplayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// firstSeen records id at now and reports whether it was new. Expired
// entries are pruned on the way in.
func (c *webhookRelayReplayCache) firstSeen(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]time.Time)
	}
	if at, ok := c.seen[id]; ok && now.Sub(at) < webhookRelayReplayTTL {
		return false
	}
	for k, at := range c.seen {
		if now.Sub(at) >= webhookRelayReplayTTL {
			delete(c.seen, k)
		}
	}
	if len(c.seen) >= webhookRelayReplayMax {
		oldestID, oldestAt := "", now
		for k, at := range c.seen {
			if oldestID == "" || at.Before(oldestAt) {
				oldestID, oldestAt = k, at
			}
		}
		delete(c.seen, oldestID)
	}
	c.seen[id] = now
	return true
}

// verifyWebhookRelay checks a hub-relayed delivery and returns "" when it is
// accepted, otherwise the rejection reason. The delivery ID is remembered
// only once the signature and timestamp verify, so unsigned traffic cannot
// fill the replay cache.
func (s *Server) verifyWebhookRelay(h http.Header, body []byte, key string, now time.Time) string {
	ts := h.Get(webhookRelayTimestampHeader)
	delivery := h.Get("X-GitHub-Delivery")
	if !verifyWebhookRelaySignature(body, ts, delivery, h.Get(webhookRelaySignatureHeader), key) {
		return github.WebhookReasonRelaySignatureFailed
	}
	if delivery == "" {
		return github.WebhookReasonRelayDeliveryMissing
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return github.WebhookReasonRelayStale
	}
	if skew := now.Sub(time.Unix(sec, 0)); skew > webhookRelayMaxSkew || skew < -webhookRelayMaxSkew {
		return github.WebhookReasonRelayStale
	}
	if !s.webhookRelaySeen.firstSeen(delivery, now) {
		return github.WebhookReasonRelayReplayed
	}
	return ""
}
