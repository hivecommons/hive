package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// hiveWriteDeliveryPrefix marks a tracker eviction that came from the hive's
// own write rather than a GitHub delivery, in the item's patch audit trail.
const hiveWriteDeliveryPrefix = "hive-write:"

// EvictHiveClosedItem drops an issue or pull request the hive itself has just
// closed or merged from the cached actionable snapshot and the published
// status, through the same patch path a `closed` webhook delivery takes.
//
// A merge the hive performs is the one close it never has to be told about,
// yet before this the row's departure depended entirely on the delivery
// coming back round through the hub (single-shot, lost across hub rollouts
// and spoke stalls), on the closed-row sweep (only on status publish, every
// 15 minutes in webhook cadence) or on the next full enumeration.
// hivecommons/docs#469 was merged by the automerge sweep at 18:25 and still
// read "Sentinel alert - needs human" on the tracker an hour later, with the
// governor re-evaluating it as merge-eligible seven more times from the
// cached snapshot. action is the audit action that recorded the write
// (pr_merged, pr_closed, ...), kept on the patch for audit. It reports
// whether a listed row was removed.
func (s *Server) EvictHiveClosedItem(repo string, number int, isPR, merged bool, action string, at time.Time) bool {
	if s == nil || number <= 0 || strings.TrimSpace(repo) == "" {
		return false
	}
	if at.IsZero() {
		at = time.Now()
	}
	snap := github.WebhookItemSnapshot{
		Repo: repo, Number: number, IsPR: isPR, State: "closed", Merged: merged,
		UpdatedAt: at, FetchedAt: at, DeliveryID: hiveWriteDeliveryPrefix + action,
	}
	evicted := s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{snap}) > 0
	if evicted && s.logger != nil {
		s.logger.Info("stage tracker evicted row the hive itself closed",
			"repo", repo, "number", number, "pr", isPR, "merged", merged, "action", action)
	}
	return evicted
}
