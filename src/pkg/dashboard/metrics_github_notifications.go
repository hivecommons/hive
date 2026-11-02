package dashboard

import (
	"fmt"
	"strings"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// githubNotificationSnapshot is what /metrics exports about the hive's
// notification-capable GitHub writes: the always-on comment tally and, when
// github.notifications.shadow is on, the shadow counters.
type githubNotificationSnapshot struct {
	Created, Edited, Suppressed int64
	Shadow                      bool
	Counts                      []ghpkg.NotificationShadowCount
	ActiveItems                 int
}

func githubNotificationSnapshotFrom(c *ghpkg.Client) githubNotificationSnapshot {
	var snap githubNotificationSnapshot
	if c == nil {
		return snap
	}
	snap.Created, snap.Edited, snap.Suppressed = c.NotificationCounts()
	snap.Counts = c.NotificationShadowCounts()
	snap.Shadow = c.NotificationShadowEnabled() || len(snap.Counts) > 0
	snap.ActiveItems = c.NotificationShadowActiveItems()
	return snap
}

var githubNotificationShadowMetricHelp = []struct{ metric, help string }{
	{ghpkg.NotificationMetricCommentsCreated, "New issue/PR comments the hive created (API writes, not emails delivered), by producer and class."},
	{ghpkg.NotificationMetricReviewsSubmitted, "PR reviews the hive submitted, by producer and class."},
	{ghpkg.NotificationMetricReviewRequests, "Review requests the hive made, by producer and class."},
	{ghpkg.NotificationMetricMentions, "@-mentions carried by comments and reviews the hive posted, by producer and class."},
	{ghpkg.NotificationMetricSuppressedRepeats, "Notes the hive chose not to post as a new comment (folded or already present), by producer and class."},
	{ghpkg.NotificationMetricWouldSuppressRepeats, "Writes shadow mode would have suppressed as a repeat of the same condition, by producer and class. Nothing is suppressed."},
	{ghpkg.NotificationMetricDeliveryFailures, "Notification-capable writes that failed, by producer and class."},
}

// writeGitHubNotificationMetrics emits hive_github_comments_total and, when
// shadow mode is or was on, the hive_github_notification_* series.
func writeGitHubNotificationMetrics(b *strings.Builder, hiveID string, writeHeader func(name, help, typ string), snap githubNotificationSnapshot) {
	writeHeader("hive_github_comments_total",
		"Hive comments since process start by operation: created (notifies), edited (does not), suppressed (folded or already present).", "counter")
	for _, op := range []struct {
		name string
		v    int64
	}{{"created", snap.Created}, {"edited", snap.Edited}, {"suppressed", snap.Suppressed}} {
		fmt.Fprintf(b, "hive_github_comments_total{hive_id=%s,op=%s} %d\n", promQuote(hiveID), promQuote(op.name), op.v)
	}
	if !snap.Shadow {
		return
	}
	for _, m := range githubNotificationShadowMetricHelp {
		name := "hive_github_notification_" + m.metric + "_total"
		writeHeader(name, m.help, "counter")
		for _, c := range snap.Counts {
			if c.Metric != m.metric {
				continue
			}
			fmt.Fprintf(b, "%s{hive_id=%s,producer=%s,class=%s} %d\n",
				name, promQuote(hiveID), promQuote(c.Producer), promQuote(string(c.Class)), c.Value)
		}
	}
	writeHeader("hive_github_notification_active_items",
		"Issues and PRs that received a notification-capable hive write in the last 24 hours; the per-active-item denominator.", "gauge")
	fmt.Fprintf(b, "hive_github_notification_active_items{hive_id=%s} %d\n", promQuote(hiveID), snap.ActiveItems)
}
