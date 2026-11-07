package dashboard

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// basePreparationQueuedPrefix is the lane reason of a PR parked in the base
// preparation train behind its front (automerge_base_preparation.go). The
// stage engine files every base-refresh reason under 050 whether or not the
// head already holds a verdict, so the 050 caption composed from the stage
// alone reads "waiting for a verdict" next to a Review 5/5 approved chevron
// (hivecommons/hive#16756: #16691 behind front #16718).
const basePreparationQueuedPrefix = "base-refresh:queued-behind#"

// stageTrackerApplyTrainQueuedCaption rewrites the caption and next-move
// line of a train-queued 050 PR once the review track on its current head
// says approved: the only thing the row waits on is its turn behind the
// front, never a verdict. A reviewing or unknown head keeps the stage's
// words.
func stageTrackerApplyTrainQueuedCaption(it *StageTrackerItem) {
	if it == nil || it.Kind != "pr" || stageByNumber(it.Stage) != stagelabels.ReviewPending {
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(it.Reason), basePreparationQueuedPrefix) {
		return
	}
	if !stageTrackerActivityOnHead(it, it.ReviewActivity) || it.ReviewActivity.State != "approved" {
		return
	}
	wait := humanizeStageReason(it.Reason)
	if wait == "" {
		return
	}
	it.Caption = stageTrackerApprovedWord(it.ReviewActivity) + " · " + wait
	it.Actor = "automerge lane"
	if it.StageActor != nil && it.StageActor.Kind == StageActorHive {
		it.StageActor.Why = "approved — " + wait
	}
}

// stageTrackerApprovedWord is the chevron's approved count as a caption
// lead ("Approved 5/5"), or a bare "Approved" when no count could be made.
func stageTrackerApprovedWord(a *StageTrackerActivity) string {
	if a == nil || a.PerspectivesTotal <= 0 {
		return "Approved"
	}
	return "Approved " + strconv.Itoa(a.PerspectivesDone) + "/" + strconv.Itoa(a.PerspectivesTotal)
}
