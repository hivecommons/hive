package dashboard

import (
	"fmt"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestWriteGitHubNotificationMetrics(t *testing.T) {
	render := func(snap githubNotificationSnapshot) string {
		var b strings.Builder
		writeHeader := func(name, help, typ string) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		}
		writeGitHubNotificationMetrics(&b, "h1", writeHeader, snap)
		return b.String()
	}

	off := render(githubNotificationSnapshot{Created: 3, Edited: 2, Suppressed: 1})
	for _, want := range []string{
		`hive_github_comments_total{hive_id="h1",op="created"} 3`,
		`hive_github_comments_total{hive_id="h1",op="edited"} 2`,
		`hive_github_comments_total{hive_id="h1",op="suppressed"} 1`,
	} {
		if !strings.Contains(off, want) {
			t.Errorf("missing %q in:\n%s", want, off)
		}
	}
	if strings.Contains(off, "hive_github_notification_") {
		t.Errorf("shadow series emitted while shadow is off:\n%s", off)
	}

	on := render(githubNotificationSnapshot{
		Shadow: true,
		Counts: []ghpkg.NotificationShadowCount{
			{Metric: ghpkg.NotificationMetricWouldSuppressRepeats, Producer: "github.(*Client).postUnblockedNote", Class: ghpkg.NotificationClassStatus, Value: 4},
			{Metric: ghpkg.NotificationMetricReviewRequests, Producer: "github.(*Client).RequestReviewers", Class: ghpkg.NotificationClassRequiredReview, Value: 1},
		},
		ActiveItems: 7,
	})
	for _, want := range []string{
		`hive_github_notification_would_suppress_repeats_total{hive_id="h1",producer="github.(*Client).postUnblockedNote",class="status"} 4`,
		`hive_github_notification_review_requests_total{hive_id="h1",producer="github.(*Client).RequestReviewers",class="required_review"} 1`,
		`# TYPE hive_github_notification_delivery_failures_total counter`,
		`hive_github_notification_active_items{hive_id="h1"} 7`,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("missing %q in:\n%s", want, on)
		}
	}
}
