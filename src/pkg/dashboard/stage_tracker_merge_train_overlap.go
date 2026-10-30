package dashboard

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerOverlap is the hint on a PR short of the merge queue whose
// files overlap another open PR's: Behind is the PR it will queue behind
// once both are merge-ready (0 when this one is the planner's front),
// With the other overlapping PRs, Files the overlapping files and Note
// the one line the row shows — "overlaps #15637 (sidebar.html,
// index.html) — will queue behind it". The row has no merge_train, no
// ordinal and no rail: the train starts at 080.
type StageTrackerOverlap struct {
	Behind int      `json:"behind,omitempty"`
	With   []int    `json:"with,omitempty"`
	Files  []string `json:"files,omitempty"`
	Note   string   `json:"note"`
	// Badge overrides the chip's words when set (an admitted candidate
	// reads "joins behind #N next pass", not "when green").
	Badge string `json:"badge,omitempty"`
	// trainID and position remember the planner's slot so the members
	// that do ride can close ranks (stageTrackerMergeTrainPostPass).
	trainID  string
	position int
}

// stageTrackerOverlapHint builds the hint for a PR at number, out of a
// train whose front is front and whose other members are others (number
// excluded by the caller). front is the PR this one joins behind when it
// is green — the ready member the lane merges first — or number itself
// when no member is ready yet: the lane orders a train readiness first
// (automerge_merge_train.go), so a PR at CI / Review never merges ahead
// of a green one; it is "1st" only provisionally, while nobody is ready.
func stageTrackerOverlapHint(number, front int, others []int, files []string) *StageTrackerOverlap {
	o := &StageTrackerOverlap{With: append([]int(nil), others...), Files: append([]string(nil), files...)}
	paren := ""
	if len(files) > 0 {
		paren = " (" + strings.Join(files, ", ") + ")"
	}
	if number != front && front > 0 {
		o.Behind = front
		o.Note = "overlaps #" + strconv.Itoa(front) + paren + " — " + stageTrackerOverlapJoinsNote
		return o
	}
	names := make([]string, 0, len(others))
	for _, n := range others {
		names = append(names, "#"+strconv.Itoa(n))
	}
	them := "it"
	if len(others) > 1 {
		them = "them"
	}
	o.Note = "overlaps " + strings.Join(names, ", ") + paren + " — " + stageTrackerOverlapProvisionalNote + " merges ahead of " + them + " if it is green first"
	return o
}

// stageTrackerOverlapProvisionalNote leads the hint on a PR the planner
// ordered first while no member of its train is merge-ready.
const stageTrackerOverlapProvisionalNote = "no member of the train is merge-ready yet; provisionally 1st,"

// stageTrackerOverlapJoinsNote is the hint's tail on a PR that joins a
// train behind its ready front once green.
const stageTrackerOverlapJoinsNote = "joins the merge train behind it when green"

// stageTrackerOverlapJoinBehind re-points a below-queue PR's hint at the
// front that actually rides: the planner may have seated this PR first
// while it was still at CI / Review, but the lane merges the ready member
// first and this one joins behind it when green (hive's own #15741 at CI /
// Review was drawn "1st" over #15748 and #15755, both green at 080).
func stageTrackerOverlapJoinBehind(it *StageTrackerItem, front int) {
	if it == nil || it.Overlap == nil || front <= 0 || front == it.Number {
		return
	}
	o := it.Overlap
	others := make([]int, 0, len(o.With)+1)
	seen := map[int]bool{it.Number: true}
	for _, n := range append([]int{front}, o.With...) {
		if !seen[n] {
			seen[n] = true
			others = append(others, n)
		}
	}
	sort.Ints(others)
	trainID, pos := o.trainID, o.position
	it.Overlap = stageTrackerOverlapHint(it.Number, front, others, o.Files)
	it.Overlap.trainID, it.Overlap.position = trainID, pos
}

// stageTrackerLeaveMergeTrainBelowQueue takes a PR short of the merge
// queue out of its train and leaves the overlap hint in its place.
func stageTrackerLeaveMergeTrainBelowQueue(it *StageTrackerItem, front int, others []int) {
	if it == nil || it.MergeTrain == nil {
		return
	}
	mt := it.MergeTrain
	pos := mt.Position
	if mt.State == MergeGateStateTrainFront {
		pos = 1
	}
	stageTrackerLeaveMergeTrain(it, "")
	it.Overlap = stageTrackerOverlapHint(it.Number, front, others, mt.OverlapFiles)
	it.Overlap.trainID, it.Overlap.position = mt.ID, pos
}

// stageTrackerTrainUnderRepair names why a PR at stage, with reason, is
// not a train member right now — "" when it may ride. A PR being fixed
// (070), waiting on its author (071) or contributor (072), freshly
// adopted (074), parked on a human decision (085), or whose required
// checks are red is somebody's to repair first; the planner may still
// have ordered it, but the view never seats it. A PR short of the merge
// queue is not under repair but rides no train either
// (stageTrackerBelowMergeQueue).
//
// stageTrackerItemColumn reads the same predicate for the chevron
// column, and stageTrackerUnderRepair in the SPA mirrors it: membership
// and column can never disagree (hive's own #15574 sat at 070 "Fixing"
// under Merge queue because its stale merge-failed gate still promoted
// the column after the train rule had already dropped it).
func stageTrackerTrainUnderRepair(stage int, reason string) string {
	switch stage {
	case stagelabels.Fixing.Number():
		return stagelabels.Fixing.Slug()
	case stagelabels.NeedsAuthor.Number():
		return stagelabels.NeedsAuthor.Slug()
	case stagelabels.ReturnedToContributor.Number():
		return stagelabels.ReturnedToContributor.Slug()
	case stagelabels.AdoptedByHive.Number():
		return stagelabels.AdoptedByHive.Slug()
	case stagelabels.NeedsDecision.Number():
		return stagelabels.NeedsDecision.Slug()
	}
	lower := strings.ToLower(strings.TrimSpace(reason))
	if strings.HasPrefix(lower, "required-check-failing:") || strings.HasPrefix(lower, "check-failing:") || lower == "ci-red" {
		return "checks-failing"
	}
	return ""
}

// stageTrackerLeaveMergeTrain takes a PR out of its train: the train
// record and the train's gate words go, and LeftTrain keeps why (and,
// unexported, which train and which slot) so the row can say so on hover
// and the survivors can close ranks. why "" (a train that shrank to one
// member) leaves no note — there is no train to have left.
func stageTrackerLeaveMergeTrain(it *StageTrackerItem, why string) {
	if it == nil || it.MergeTrain == nil {
		return
	}
	mt := it.MergeTrain
	if it.MergeGate != nil && (it.MergeGate.State == mt.State || it.MergeGate.State == MergeGateStateTrainFront) {
		// Back to the stage's own gate state (ready at 080, pending at
		// 060); off the gate there is no gate.
		it.MergeGate.State = ""
		switch it.Stage {
		case stagelabels.Reviewed.Number():
			it.MergeGate.State = MergeGateStatePending
		case stagelabels.AwaitingMergeAdmission.Number():
			it.MergeGate.State = MergeGateStateReady
		}
		it.MergeGate.Reason = ""
		if it.MergeGate.State == "" || reflect.DeepEqual(*it.MergeGate, StageTrackerMergeGate{}) {
			it.MergeGate = nil
		}
	}
	it.MergeTrain = nil
	// The caption the train wrote ("waiting its turn … behind #N") is
	// stale now; an 080 row goes back to the gate's own words.
	if it.Stage == stagelabels.AwaitingMergeAdmission.Number() && (it.Caption == stageTrackerMergeTrainCaption(it.Reason) || it.Caption == stageTrackerMergeTrainFrontCaption(mt)) {
		it.Caption = stageTrackerAwaitingAdmissionCaption
	}
	if why != "" {
		pos := mt.Position
		if mt.State == MergeGateStateTrainFront {
			pos = 1
		}
		it.LeftTrain = &StageTrackerLeftTrain{Why: why, Note: stageTrackerLeftTrainNote, trainID: mt.ID, position: pos}
	}
}
