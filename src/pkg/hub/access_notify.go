package hub

// Owner notification for new access requests (issue #4149).
//
// When a user files a NEW pending access request for a hive, the hive's owner
// gets a push notification through the hub's existing notification config: a
// Slack DM via the same HIVE_HUB_SLACK_BOT_TOKEN + slack_id plumbing the
// operator messaging endpoints use (slack.go). There is no hub-side email
// sender, so Slack is the only push channel; the dashboard's pending-requests
// banner remains the in-app notification either way.
//
// Deduplication is inherited, not reimplemented: handleRequestAccess rejects a
// request while one from the same user is already pending, and this notifier
// only runs after a request is actually persisted — so a requester hammering
// the endpoint can never generate more than one notification per pending
// request.

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// hubDashboardBaseURL resolves the hub's public origin for building links in
// notifications. Same env-var precedence as hubDomainSuffix
// (url_reachability.go); falls back to the canonical public origin.
func hubDashboardBaseURL() string {
	for _, key := range []string{"HIVE_HUB_PUBLIC_URL", "HIVE_PUBLIC_URL", "HIVE_HUB_BASE_URL", "HIVE_DASHBOARD_URL", "HIVE_HUB_URL"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return hubPublicURL()
}

// accessRequestReviewLink builds the one-click deep link that opens the
// dashboard's Manage Access modal for the hive (handled client-side by the
// manage_access query param, next to the existing request_hive deep link).
func accessRequestReviewLink(hiveID string) string {
	return hubDashboardBaseURL() + "/dashboard?manage_access=" + url.QueryEscape(hiveID)
}

// notifyOwnerAccessRequest DMs the hive owner about a newly filed pending
// access request. Every skip is logged, never silent: no owner, no bot token,
// no user record and no slack_id are all expected states for some hives, and
// the log line is how an operator learns why a notification did not go out.
//
// The actual send runs on a background goroutine (deliverSlackMessages), so
// this never delays the requester's HTTP response.
func (s *HubServer) notifyOwnerAccessRequest(hiveID, owner, requester, note string) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		// An unclaimed placeholder has no owner to notify.
		return
	}
	token := strings.TrimSpace(os.Getenv(slackTokenEnvVar))
	if token == "" {
		s.logger.Info("access request notification skipped: no slack bot token configured",
			"hive", hiveID, "owner", owner, "requester", requester)
		return
	}
	u := loadSaaSUser(owner)
	if u == nil {
		s.logger.Warn("access request notification skipped: owner has no user record",
			"hive", hiveID, "owner", owner, "requester", requester)
		return
	}
	recipients, _ := resolveSlackRecipients([]SaaSUser{*u})
	if len(recipients) == 0 {
		s.logger.Info("access request notification skipped: owner has no slack_id",
			"hive", hiveID, "owner", owner, "requester", requester)
		return
	}

	message := fmt.Sprintf(
		"🔑 New access request for hive %s\n%s requested access: %q\nReview and approve: %s",
		hiveID, requester, note, accessRequestReviewLink(hiveID))

	s.logger.Info("audit: access request notification queued",
		"hive", hiveID, "owner", owner, "requester", requester)
	go s.deliverSlackMessages(token, "access-request", message, recipients, requester)
}
