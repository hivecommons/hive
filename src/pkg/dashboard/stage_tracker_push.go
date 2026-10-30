package dashboard

import (
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Push progress (hivecommons/hive#15784): a PR row waiting on its author
// to push used to read the same until the review / CI machinery caught
// up. The webhook re-read after a `pull_request` synchronize already
// carries the new head, so the stage record keeps it (Store.NotePush) and
// the tracker row exposes it next to when the wait began.

// noteWebhookPushes records, for every open PR re-read whose head moved,
// the push on the item's stage record. Only a `pull_request` synchronize
// moves a head, so a re-read showing one the record does not know is
// that push, whichever delivery asked for the read.
func (s *Server) noteWebhookPushes(snaps []github.WebhookItemSnapshot) {
	store := s.stageStoreOrShared()
	if store == nil {
		return
	}
	for _, snap := range snaps {
		if !snap.IsPR || !snap.Open() || snap.HeadSHA == "" || snap.Repo == "" || snap.Number <= 0 {
			continue
		}
		at := snap.FetchedAt
		if at.IsZero() {
			at = time.Now()
		}
		if store.NotePush(stagelabels.Key(snap.Repo, snap.Number), snap.HeadSHA, at) {
			s.webhookLogger().Debug("stage tracker push recorded", "repo", snap.Repo, "number", snap.Number, "head", snap.HeadSHA, "delivery", snap.DeliveryID)
		}
	}
}

// stageTrackerPushProgress copies rec's push progress onto a PR row.
func stageTrackerPushProgress(it *StageTrackerItem, rec stagelabels.Record) {
	if it == nil || it.Kind != "pr" {
		return
	}
	if !rec.WaitingPushSince.IsZero() {
		t := rec.WaitingPushSince
		it.WaitingPushSince = &t
	}
	if rec.LastPushSHA != "" && !rec.LastPushAt.IsZero() {
		t := rec.LastPushAt
		it.LastPushAt = &t
		it.LastPushSHA = rec.LastPushSHA
	}
	it.PushedSinceWaiting = rec.PushedSinceWaiting()
}
