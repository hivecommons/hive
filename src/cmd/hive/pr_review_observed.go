package main

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// auditAtSink is the slice of *dashboard.Server recordObservedReview writes
// to: an audit entry stamped at a given instant.
type auditAtSink interface {
	AuditLogRecordAt(timestamp, user, action, detail, agent, repo string, target int)
}

// recordObservedReview records a review someone other than the hive
// submitted on a terminal PR (hivecommons/hive#16116) as pr_review_observed,
// at the review's own submitted_at so the throughput card buckets it when the
// review happened, not when the settle scan saw it. review_id is the counters'
// de-duplication key and actor_class their human/other attribution, so an
// observation missing either an ID or a submit time is not recorded.
func recordObservedReview(sink auditAtSink, obs github.PRReviewObservation) {
	if sink == nil || obs.ReviewID == 0 || obs.SubmittedAt.IsZero() {
		return
	}
	sink.AuditLogRecordAt(obs.SubmittedAt.UTC().Format(time.RFC3339), "system", github.AuditActionPRReviewObserved,
		fmt.Sprintf("repo=%s, number=%d, review_id=%d, actor=%s, actor_class=%s", obs.Repo, obs.Number, obs.ReviewID, obs.Reviewer, obs.Attribution),
		github.AttributionAgentGovernor, obs.Repo, obs.Number)
}
