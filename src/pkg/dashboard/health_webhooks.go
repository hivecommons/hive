package dashboard

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/github"
)

// Webhook receiver health. A hosted spoke that never received
// GITHUB_WEBHOOK_SECRET answers every relayed delivery with 503 and stays on
// the polling cadence; before this check the only trace was one WARN line
// per process, and /api/status read the same as "GitHub is not sending".
// The check names the cause (webhooks.mode_reason_code) as a warning: the
// hive still works on polling, so it is never a failure and never raises the
// hub's health-check-failing alert.
//
// Only whether the secret is set is ever reported, never its value, length
// or a digest of it.

const healthCheckWebhooks = "webhooks"

// webhookHealth grades a webhook snapshot: "warn" when deliveries arrive but
// cannot be used, or webhooks degraded while GitHub stayed busy; "pass"
// otherwise, with the mode and cause in the detail.
func webhookHealth(snap github.WebhookHealth) (status, detail string) {
	// A hosted spoke verifies hub relays with its relay key (#17911); name
	// that path so it is not mistaken for a spoke holding the App secret.
	via := ""
	if snap.Verification == github.WebhookVerificationRelay {
		via = " via " + github.WebhookVerificationRelay
	}
	if snap.Mode != github.WebhookModePolling {
		return "pass", fmt.Sprintf("webhook mode%s — %d deliveries in the last hour", via, snap.Events1h)
	}
	rejected := fmt.Sprintf("%d rejected in the last hour", snap.Rejected1h)
	switch snap.ReasonCode {
	case github.WebhookReasonRelaySignatureFailed:
		return "warn", "polling — hub-relayed deliveries fail the relay signature: this spoke's relay key does not match the hub's (" + rejected + ")"
	case github.WebhookReasonRelayStale:
		return "warn", "polling — hub-relayed deliveries arrive with stale timestamps: check the hub and spoke clocks (" + rejected + ")"
	case github.WebhookReasonRelayReplayed, github.WebhookReasonRelayDeliveryMissing:
		return "warn", "polling — hub-relayed deliveries rejected (" + snap.ReasonCode + ", " + rejected + ")"
	case github.WebhookReasonNoSecret:
		// Only rejections inside the window count: one delivery days ago must
		// not leave the spoke at warn for good.
		if snap.Rejected1h > 0 {
			return "warn", "polling — no GITHUB_WEBHOOK_SECRET on this spoke; relayed deliveries are rejected with 503 (" + rejected + ")"
		}
		return "pass", "polling — no GITHUB_WEBHOOK_SECRET on this spoke; webhooks are off and no delivery was rejected in the last hour"
	case github.WebhookReasonSignatureFailed:
		return "warn", "polling — deliveries fail signature verification: GITHUB_WEBHOOK_SECRET does not match the secret the sender signs with (" + rejected + ")"
	case github.WebhookReasonDeliveriesRejected:
		return "warn", "polling — deliveries arrive but are rejected as undecodable payloads (" + rejected + ")"
	case github.WebhookReasonNoDeliveries:
		if snap.Reason != "" {
			return "warn", "polling — " + snap.Reason + "; check the GitHub App webhook URL or the hub relay"
		}
		return "pass", "polling — no verified delivery yet"
	}
	if snap.HealthyWindows > 0 {
		return "pass", fmt.Sprintf("polling%s — recovering, %d of %d healthy windows", via, snap.HealthyWindows, github.WebhookRecoveryHealthyWindows)
	}
	return "pass", "polling" + via + " — verified deliveries arriving; webhook mode engages at the next eval cycle"
}

// webhookHealthCheck grades the live webhook tracker.
func webhookHealthCheck() (status, detail string, snap github.WebhookHealth) {
	snap = github.WebhookHealthSnapshot()
	status, detail = webhookHealth(snap)
	return status, detail, snap
}
