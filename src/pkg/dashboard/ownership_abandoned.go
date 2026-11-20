package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// OwnershipReasonAbandonedRecommendedClose: a contributor PR parked at 071
// needs-author or at an adoption-stalled 085 sat with no outside activity
// past governor.labels.stage.abandon_after. Nothing the hive can do moves
// it, so a person closes it as stale or takes it over; the row is a CLOSE
// so it lands in the inbox's "to close" group and the review_queue MCP's
// to_close count instead of among live decisions.
const OwnershipReasonAbandonedRecommendedClose = "abandoned_recommended_close"

func init() {
	ownershipReasonVerbs[OwnershipReasonAbandonedRecommendedClose] = OwnershipVerbClose
}

// abandonedPRFor is the ownership pass's check on a listed PR: the stage
// store's abandoned flag, rendered as "Stale: close or take over —
// abandoned 9d: <blocker>". A short repo name is qualified with org, as
// adoptedPushRefusalFor does.
func abandonedPRFor(org string, now time.Time) func(pr github.PullRequest) (string, bool) {
	return func(pr github.PullRequest) (string, bool) {
		store := adoptedPushStore()
		if store == nil || pr.Number <= 0 {
			return "", false
		}
		repo := strings.TrimSpace(pr.Repo)
		if !strings.Contains(repo, "/") && strings.TrimSpace(org) != "" {
			repo = strings.TrimSpace(org) + "/" + repo
		}
		rec, ok := store.Get(stagelabels.Key(repo, pr.Number))
		if !ok || !rec.Abandoned() {
			return "", false
		}
		return abandonedOwnershipText(rec.AbandonedReason(now)), true
	}
}

// abandonedOwnershipText is the row's reason: the group label, then the
// abandoned reason with its original blocker.
func abandonedOwnershipText(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "Stale: close or take over"
	}
	return "Stale: close or take over — " + reason
}

// stageTrackerAbandonedCaption is the stage tracker row's caption for an
// abandoned contributor PR: the group it sits in, then how long and on what.
func stageTrackerAbandonedCaption(reason string) string {
	return abandonedOwnershipText(reason) + "; a person decides"
}
