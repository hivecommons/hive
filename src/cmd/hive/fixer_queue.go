package main

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// reviewFixKickQueue maps delivery outcomes back to PRs. Review and routine
// governor kicks must not create or clear a PR's deferred-fixer hint.
type reviewFixKickQueue struct {
	governor *governor.Governor
	kicks    map[string]review.DispatchKick
}

func (q reviewFixKickQueue) Deferred(msg scheduler.KickMessage) {
	if k, ok := q.kicks[reviewKickKey(msg)]; ok && k.Kind == "fix" {
		q.governor.RecordFixKickDeferred(k.PRRef, time.Now())
	}
}

func (q reviewFixKickQueue) Delivered(msg scheduler.KickMessage) {
	if k, ok := q.kicks[reviewKickKey(msg)]; ok && k.Kind == "fix" {
		q.governor.RecordFixKickDelivered(k.PRRef)
	}
}

func reconcileDeferredFixQueue(gov *governor.Governor, org string, actionable *github.ActionableResult) {
	if actionable == nil {
		return // no new enumeration is not evidence that a PR closed
	}
	open := map[string]bool{}
	for _, prs := range [][]github.PullRequest{actionable.PRs.Items, actionable.PRs.Held, actionable.PRs.StaleDrafts, actionable.PRs.Attributed} {
		for _, pr := range prs {
			if strings.EqualFold(pr.State, "closed") || !pr.ClosedAt.IsZero() || !pr.MergedAt.IsZero() {
				continue
			}
			repo := pr.Repo
			if org != "" && !strings.Contains(repo, "/") {
				repo = org + "/" + repo
			}
			open[github.ReviewQueueKey(repo, pr.Number)] = true
		}
	}
	gov.ReconcileFixerQueue(open)
}
