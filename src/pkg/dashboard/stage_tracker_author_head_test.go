package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerNewHeadSynchronizesAuthorWaitAndReview(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	requested := now.Add(-time.Hour)
	for _, stage := range []stagelabels.Stage{stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor} {
		t.Run(stage.String(), func(t *testing.T) {
			rec := stagelabels.Record{Stage: stage, EnteredAt: requested, NudgeHead: "old-head",
				FirstNudgeAt: requested, LastNudgeAt: requested, WaitingPushSince: requested,
				ActivityHead: "old-head", Reason: "contributor-lane:changes-requested"}
			it := StageTrackerItem{Kind: "pr", Repo: "o/r", Number: 16012, Head: "new-head", Stage: stage.Number(),
				Author: "clubanderson", AuthorKind: authorKindHuman, Reason: rec.Reason, URL: "https://github.com/o/r/pull/16012"}
			it.Push = stageTrackerPushFor(it, stage, rec, true, time.Hour, now)
			if it.Push == nil || it.Push.State != pushStatePushed || it.Push.Sha != it.Head || it.Push.Next != pushNextReReview || it.Push.Since != nil {
				t.Fatalf("new head must satisfy the old request without inventing a push time: %+v", it.Push)
			}

			it.StageActor = stageTrackerStageActor(it, nil, now)
			if it.StageActor.Kind != StageActorHive || !strings.Contains(it.StageActor.Why, "re-review") {
				t.Fatalf("new head still waits on its author: %+v", it.StageActor)
			}
			if stageTrackerNeedsHuman(it) || stageTrackerHumanAttention(it) != nil || stageTrackerApprovalReviewBlocker(it) != "" {
				t.Fatalf("old-head author/review blocker survived the push: %+v", it)
			}
			if caption := stageTrackerPushCaption(it.Push, now); caption != "new head observed → re-review pending" {
				t.Fatalf("caption contradicts pending re-review: %q", caption)
			}
			rework := stageTrackerReworkFor(it, it.Author, stage, rec, true, nil, now)
			if rework == nil || rework.State != reworkStatePushed || rework.Head != it.Head {
				t.Fatalf("event path disagrees with the canonical push: %+v", rework)
			}
			// A sweep evaluated this head and returned it to the author for
			// another action. Its fresh wait outranks the old nudge.
			rec.ActivityHead, rec.WaitingPushSince = it.Head, now.Add(-time.Minute)
			it.Push = stageTrackerPushFor(it, stage, rec, true, time.Hour, now)
			if it.Push == nil || it.Push.State != pushStateWaiting || !it.Push.Since.Equal(rec.WaitingPushSince) {
				t.Fatalf("current-head author request was hidden: %+v", it.Push)
			}
			rework = stageTrackerReworkFor(it, it.Author, stage, rec, true, nil, now)
			if rework == nil || rework.State == reworkStatePushed {
				t.Fatalf("current-head wait falsely emits a pushed event: %+v", rework)
			}
			it.StageActor = stageTrackerStageActor(it, nil, now)
			if it.StageActor.Kind != StageActorHuman || !stageTrackerNeedsHuman(it) || stageTrackerHumanAttention(it) == nil {
				t.Fatalf("independent current-head author work was lost: %+v", it)
			}
			// A pending exact-head review is not an author hold.
			rec.WaitingOnHive = true
			it.Push = stageTrackerPushFor(it, stage, rec, true, time.Hour, now)
			if it.Push == nil || it.Push.State != pushStatePushed {
				t.Fatalf("hive-side review still claims to wait on author: %+v", it.Push)
			}
		})
	}
}
