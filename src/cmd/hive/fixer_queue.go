package main

import (
	"strconv"
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
	org      string
	kicks    map[string]review.DispatchKick
}

func (q reviewFixKickQueue) Deferred(msg scheduler.KickMessage) {
	if k, ok := q.kicks[reviewKickKey(msg)]; ok && k.Kind == "fix" {
		q.governor.RecordFixKickDeferred(fixerQueueKey(q.org, k), time.Now())
	}
}

func (q reviewFixKickQueue) Delivered(msg scheduler.KickMessage) {
	if k, ok := q.kicks[reviewKickKey(msg)]; ok && k.Kind == "fix" {
		q.governor.RecordFixKickDelivered(fixerQueueKey(q.org, k))
	}
}

// fixerQueueKey is the governor's identity for a fix kick's PR. Dispatch plans
// carry pr.Repo as configured (often the bare "repo"), while the enumeration
// reconcile and /api/review/queue key by github.ReviewQueueKey("org/repo", N).
// Every writer and reader must agree on the qualified form, or a bare-repo
// entry is evicted on the next reconcile and the dashboard never finds it.
func fixerQueueKey(org string, k review.DispatchKick) string {
	repo, number := k.Repo, k.Number
	if repo == "" || number == 0 {
		// Older callers (and tests) only populate PRRef.
		ref := strings.TrimSpace(k.PRRef)
		if i := strings.LastIndex(ref, "#"); i > 0 {
			repo = ref[:i]
			if n, err := strconv.Atoi(ref[i+1:]); err == nil {
				number = n
			}
		}
	}
	repo = strings.TrimSpace(repo)
	if org != "" && repo != "" && !strings.Contains(repo, "/") {
		repo = org + "/" + repo
	}
	return github.ReviewQueueKey(repo, number)
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

// reconcilePlannedFixQueue drops deferred fixes the current plan no longer
// kicks (new head, review passed, lane reassigned away) for PRs that stay
// open; otherwise they would sit in the queue and inflate every "ahead".
func reconcilePlannedFixQueue(gov *governor.Governor, org string, actionable *github.ActionableResult, fixKicks []review.DispatchKick) {
	if actionable == nil {
		return // no plan was built, so nothing says the fix is no longer needed
	}
	planned := map[string]bool{}
	for _, k := range fixKicks {
		planned[fixerQueueKey(org, k)] = true
	}
	gov.ReconcileFixerQueue(planned)
}
