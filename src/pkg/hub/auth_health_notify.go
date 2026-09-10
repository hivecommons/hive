package hub

// Owner notification for a fleet-wide agent backend-auth outage (#6558).
//
// When a hive's aggregate auth_health transitions to "down" — every enabled
// agent has been failing backend auth for longer than AuthHealthDownThreshold
// — the hive's owner gets a push notification through the SAME channel
// notifyOwnerAccessRequest (access_notify.go, #4149) already uses: a Slack DM
// via HIVE_HUB_SLACK_BOT_TOKEN + slack_id. There is no hub-side email sender,
// so this deliberately does not add one; Slack is the only push channel the
// hub has, and the dashboard's fleet badge is the in-app fallback either way.
//
// Deduplication is the caller's job, not this file's: handleHeartbeat only
// calls notifyOwnerAuthHealthDown on the ok/degraded -> down EDGE (comparing
// this beat's entry.AuthHealth against the previous beat's), so a hive that
// stays down for hours generates exactly one notification, and the next one
// fires only after a genuine recovery (auth_health back to ok) and a second
// independent failure.
//
// TODO(#6558 follow-up): this is a single best-effort Slack DM with no retry
// and no delivery confirmation loop back into the dashboard. If Slack
// delivery itself fails (bad token, rate limited, owner has no slack_id) the
// hub logs it and moves on — an operator watching the hub's own logs is the
// only backstop. A durable notification queue / read-receipt is out of scope
// here and worth its own issue if outages like #6500 recur despite this.

import (
	"fmt"
	"os"
	"strings"
)

// notifyOwnerAuthHealthDown DMs the hive owner when the fleet-wide
// backend-auth canary (#6558) transitions to down. Every skip is logged,
// never silent — same discipline as notifyOwnerAccessRequest beside it.
func (s *HubServer) notifyOwnerAuthHealthDown(entry RegistryEntry) {
	owner := strings.TrimSpace(entry.Owner)
	if owner == "" {
		// An unclaimed placeholder, or a bare/BYO hive with no owner on
		// record, has no one to notify.
		s.logger.Warn("auth-health-down notification skipped: hive has no owner on record",
			"hive", entry.ID, "reason", entry.AuthHealthReason)
		return
	}
	token := strings.TrimSpace(os.Getenv(slackTokenEnvVar))
	if token == "" {
		s.logger.Warn("auth-health-down notification skipped: no slack bot token configured",
			"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
		return
	}
	u := loadSaaSUser(owner)
	if u == nil {
		s.logger.Warn("auth-health-down notification skipped: owner has no user record",
			"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
		return
	}
	recipients, _ := resolveSlackRecipients([]SaaSUser{*u})
	if len(recipients) == 0 {
		s.logger.Warn("auth-health-down notification skipped: owner has no slack_id",
			"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
		return
	}

	message := fmt.Sprintf(
		"🚨 Hive %s: every enabled agent has failed backend auth for over %s\n%s\nOpen the hive dashboard: %s",
		entry.ID, AuthHealthDownThreshold().String(), entry.AuthHealthReason, hubDashboardBaseURL()+"/dashboard?manage_access="+entry.ID)

	s.logger.Warn("audit: auth-health-down notification queued",
		"hive", entry.ID, "owner", owner, "reason", entry.AuthHealthReason)
	go s.deliverSlackMessages(token, "auth-health-down", message, recipients, "hive-auth-canary")
}
