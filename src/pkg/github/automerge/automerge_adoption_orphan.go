package automerge

import (
	"context"
	"fmt"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const adoptionChildClosedTwice = "adoption-child-closed-twice"

// Restore ownership from public evidence when an old sweep cleared the store.
// A marker's head is retained even when the contributor subsequently pushed:
// the merged-child path must still check whether it carried that new head.
func (c *Engine) restoreAdoptionMarker(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, now time.Time) error {
	rec, ok := store.Get(key)
	if rec.AdoptedBy > 0 || rec.AdoptedInPlace() || !obs.Detail.CommentsKnown || obs.Detail.Comments == 0 ||
		(rec.AdoptMarkerChecked && rec.AdoptMarkerComments == obs.Detail.Comments) {
		return nil
	}
	marker, err := hgithub.ReadSupersedeAdoption(ctx, c.gh, obs.Owner, obs.Repo, obs.Number, c.transport.IsHiveAuthorLogin)
	if err != nil {
		return err
	}
	if !ok {
		store.Observe(key, stagelabels.None, "")
	}
	if marker.By > 0 {
		store.MarkAdopted(key, marker.Head, marker.By, now)
	}
	store.NoteAdoptionMarkerRead(key, obs.Detail.Comments)
	return nil
}

func (c *Engine) recoverOrphanedAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, now time.Time) (int, error) {
	store.NoteAdoptionOrphan(key, rec.AdoptedBy, now, false)
	if rec.AdoptedBy != rec.AdoptOrphanPrevious && !rec.AdoptOrphanRetriedAt.IsZero() && now.Sub(rec.AdoptOrphanRetriedAt) < time.Hour {
		store.MarkAdoptionEscalated(key, obs.HeadSHA, adoptionChildClosedTwice)
		body := fmt.Sprintf("<!-- hive:adoption-orphan-parked -->\nThe hive's replacement #%d was closed again within an hour. Paused at 085: %s. A person must decide whether to resume adoption.", rec.AdoptedBy, adoptionChildClosedTwice)
		_, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)})
		if err != nil {
			return 0, err
		}
		return 1, nil
	}
	if obs.HeadSHA != rec.AdoptedHead || !policy.Adoption || adoptionBlocked(obs.Labels) != "" {
		return c.revertOrphanedAdoption(ctx, store, key, obs, rec.AdoptedBy, "the contributor head changed, adoption is disabled, or a hold label prevents takeover")
	}
	store.NoteAdoptionOrphan(key, rec.AdoptedBy, now, true)
	retry, _ := store.Get(key)
	// All publishing fences and mutation gates remain in supersedeAdoptedPR.
	n, err := c.supersedeAdoptedPR(ctx, store, key, obs, retry, policy, nonEmpty(rec.AdoptReason, AdoptReasonOffline), 0, false, false, now)
	after, _ := store.Get(key)
	if after.AdoptedBy > 0 && after.AdoptedBy != rec.AdoptedBy {
		c.info("stage labels: adoption orphaned, re-adopting", "repo", obs.Display, "pr", obs.Number, "old", rec.AdoptedBy, "new", after.AdoptedBy)
		return n, err
	}
	reverted, revertErr := c.revertOrphanedAdoption(ctx, store, key, obs, rec.AdoptedBy, "the hive could not open a fresh copy; the original is back in its pre-adoption flow")
	if err != nil {
		return n + reverted, err
	}
	return n + reverted, revertErr
}

func (c *Engine) revertOrphanedAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, previous int, reason string) (int, error) {
	body := fmt.Sprintf("<!-- hive:adoption-reverted -->\nThe hive's earlier copy #%d was closed without merging. Adoption reverted: %s. This PR remains open for its author and normal review.", previous, reason)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return 0, err
	}
	rec, _ := store.Get(key)
	store.RevertOrphanAdoption(key, nonEmpty(rec.AdoptedHead, obs.HeadSHA))
	return 1, nil
}
