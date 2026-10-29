package dashboard

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Merge-gate states a PR riding a merge train reports (hivecommons/hive#15117).
// They extend the pipeline states in stage_tracker_merge_gate.go: a train
// member at 080 is not "ready" — it is waiting its turn behind the front.
const (
	// MergeGateStateQueuedBehind: a sibling parked behind the train's front.
	MergeGateStateQueuedBehind = prstage.MergeTrainStateQueuedBehind
	// MergeGateStateRebasing: the front merged and this PR's rebase was
	// requested (attempt n of max).
	MergeGateStateRebasing = prstage.MergeTrainStateRebasing
	// MergeGateStateRebaseExhausted: the rebase failed max times; a person
	// rebases and merges.
	MergeGateStateRebaseExhausted = prstage.MergeTrainStateRebaseExhausted
	// MergeGateStateTrainFront: the PR leads its train and is merging first.
	MergeGateStateTrainFront = prstage.MergeTrainStateFront
)

// StageTrackerMergeTrain is the row's place in a conflict-aware merge
// train: PRs to one base whose changed files overlap, merged one front at
// a time (automerge_merge_train.go). The Activity Monitor draws the
// train's rows contiguous in merge order under one header and words the
// pill from State ("In merge train · 3rd of 4").
type StageTrackerMergeTrain struct {
	// ID names the train across its members: "<base>#<front>".
	ID string `json:"id"`
	// State is one of front, queued-behind, rebasing, rebase-exhausted.
	State string `json:"state"`
	// Front is the PR merging first; Position is this PR's 1-based slot
	// of Size in merge order.
	Front    int `json:"front"`
	Position int `json:"position"`
	Size     int `json:"size"`
	// Attempt / MaxAttempts count the rebase loop guard when State is
	// rebasing or rebase-exhausted.
	Attempt     int `json:"attempt,omitempty"`
	MaxAttempts int `json:"max_attempts,omitempty"`
	// OverlapFiles are the changed-file basenames the train's members
	// share (the reason the PRs cannot merge in parallel).
	OverlapFiles []string `json:"overlap_files,omitempty"`
	// Ahead lists the PRs still to merge before this one, in order.
	Ahead []int `json:"ahead,omitempty"`
}

// stageTrackerMergedStage is the terminal strip number (the dashboard's
// STAGE_TRACKER_TERMINAL); no open item carries it.
const stageTrackerMergedStage = 110

// stageTrackerMergeTrainFrom reads the train the stage reason encodes, or
// nil when the reason is not a train reason.
func stageTrackerMergeTrainFrom(reason string) *StageTrackerMergeTrain {
	mt, ok := prstage.ParseMergeTrain(reason)
	if !ok {
		return nil
	}
	return &StageTrackerMergeTrain{
		State:        mt.State,
		Front:        mt.Front,
		Position:     mt.Position,
		Size:         mt.Size,
		Attempt:      mt.Attempt,
		MaxAttempts:  mt.MaxAttempts,
		OverlapFiles: append([]string(nil), mt.OverlapFiles...),
	}
}

// stageTrackerApplyMergeTrain decorates a PR row the lane parked in a
// train: the merge-gate state becomes the train state with the sentence
// as reason, and a `needs-rebase` label stops reading as a human flag —
// the train is repairing it (it stays in StaleLabels so the hover still
// names it). The exhausted state keeps the label: that row is a person's.
func stageTrackerApplyMergeTrain(it *StageTrackerItem) {
	if it == nil || it.Kind != "pr" {
		return
	}
	mt := stageTrackerMergeTrainFrom(it.Reason)
	if mt == nil {
		return
	}
	mt.ID = it.BaseRef + "#" + strconv.Itoa(mt.Front)
	it.MergeTrain = mt
	parsed, _ := prstage.ParseMergeTrain(it.Reason)
	if it.MergeGate == nil {
		it.MergeGate = &StageTrackerMergeGate{}
	}
	it.MergeGate.State = mt.State
	it.MergeGate.Reason = parsed.Sentence()
	if mt.State != MergeGateStateRebaseExhausted && slices.Contains(it.Modifiers, modNeedsRebase) {
		it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsRebase })
		if !slices.Contains(it.StaleLabels, modNeedsRebase) {
			it.StaleLabels = append(it.StaleLabels, modNeedsRebase)
		}
	}
}

// stageTrackerMergeTrainPostPass runs over the finished items: the front
// (which the lane evaluates on its normal path and so carries no train
// reason) is marked from its siblings' records, and every member learns
// the PRs ahead of it. Members are matched on repo and base.
func stageTrackerMergeTrainPostPass(items []StageTrackerItem) {
	type trainKey struct{ repo, id string }
	members := map[trainKey][]int{}
	for i := range items {
		it := &items[i]
		if it.MergeTrain == nil || it.Kind != "pr" {
			continue
		}
		members[trainKey{it.Repo, it.MergeTrain.ID}] = append(members[trainKey{it.Repo, it.MergeTrain.ID}], i)
	}
	for key, idx := range members {
		sample := items[idx[0]].MergeTrain
		frontIdx := -1
		for i := range items {
			it := &items[i]
			if it.Kind == "pr" && it.Repo == key.repo && it.Number == sample.Front && it.Stage < stageTrackerMergedStage {
				frontIdx = i
				break
			}
		}
		if frontIdx >= 0 && items[frontIdx].MergeTrain == nil {
			front := &items[frontIdx]
			front.MergeTrain = &StageTrackerMergeTrain{
				ID: sample.ID, State: MergeGateStateTrainFront, Front: sample.Front, Position: 1, Size: sample.Size,
				OverlapFiles: append([]string(nil), sample.OverlapFiles...),
			}
			if front.MergeGate == nil {
				front.MergeGate = &StageTrackerMergeGate{}
			}
			if front.MergeGate.State == "" || front.MergeGate.State == MergeGateStateReady {
				front.MergeGate.State = MergeGateStateTrainFront
			}
			if front.MergeGate.Reason == "" {
				front.MergeGate.Reason = "front of merge train (1st of " + strconv.Itoa(sample.Size) + ", overlapping files: " + strings.Join(sample.OverlapFiles, ", ") + ")"
			}
			if front.Stage == stagelabels.AwaitingMergeAdmission.Number() {
				front.Caption = stageTrackerMergeTrainFrontCaption(sample)
			}
			idx = append(idx, frontIdx)
		}
		// Members in merge order, so each one's Ahead is the prefix.
		sort.Slice(idx, func(a, b int) bool {
			return items[idx[a]].MergeTrain.Position < items[idx[b]].MergeTrain.Position
		})
		for n, i := range idx {
			var ahead []int
			for _, j := range idx[:n] {
				ahead = append(ahead, items[j].Number)
			}
			items[i].MergeTrain.Ahead = ahead
		}
	}
}

// stageTrackerMergeTrainFrontCaption is the 080 caption of a train's front.
func stageTrackerMergeTrainFrontCaption(mt *StageTrackerMergeTrain) string {
	files := ""
	if len(mt.OverlapFiles) > 0 {
		files = " (overlapping files: " + strings.Join(mt.OverlapFiles, ", ") + ")"
	}
	return "Front of a merge train of " + strconv.Itoa(mt.Size) + " PRs; merges first, then the lane updates or rebases the others" + files
}

// stageTrackerMergeTrainCaption is the caption for a sibling's train
// reason at 080 / 085 ("" when the reason is not a train reason).
func stageTrackerMergeTrainCaption(reason string) string {
	mt, ok := prstage.ParseMergeTrain(reason)
	if !ok {
		return ""
	}
	onto := "the base"
	if mt.Front > 0 {
		onto = "the merged front #" + strconv.Itoa(mt.Front)
	}
	switch mt.State {
	case prstage.MergeTrainStateRebasing:
		return "Conflicts with " + onto + "; the hive is rebasing this PR (attempt " + strconv.Itoa(mt.Attempt) + " of " + strconv.Itoa(mt.MaxAttempts) + ")"
	case prstage.MergeTrainStateRebaseExhausted:
		return "The rebase onto " + onto + " failed " + strconv.Itoa(mt.Attempt) + " times; a maintainer rebases and merges"
	default:
		return "Approved; waiting its turn in the merge train — " + mt.Sentence()
	}
}

// decisionForMergeTrain: the train gave up rebasing this PR after the loop
// guard (hivecommons/hive#15117). The lead option is the rebase-then-merge
// follow-through (hivecommons/hive#15104): the click records the approval,
// re-opens the rebase episode in the operator's name and merges on green.
func decisionForMergeTrain(it StageTrackerItem, held bool) *StageTrackerDecision {
	mt := it.MergeTrain
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByLane, BlockerReason: it.Reason}
	onto := "the base"
	if mt.Front > 0 {
		onto = "the merged front #" + strconv.Itoa(mt.Front)
	}
	d.Question = "The merge train could not rebase this PR onto " + onto + " after " + strconv.Itoa(mt.Attempt) + " attempts (overlapping files: " + strings.Join(mt.OverlapFiles, ", ") + "). Rebase it by hand and merge as maintainer, or close it?"
	d.Blocker = "The rebase failed " + strconv.Itoa(mt.Attempt) + " of " + strconv.Itoa(mt.MaxAttempts) + " times; the train does not retry past the loop guard."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	mergeNow := decisionOption(decisionActionMergeNow, rebaseMergeFollowThroughLabel, !held, decisionWhyHeld)
	mergeNow.Consequence = rebaseMergeFollowThroughConsequence(it.Number, strings.TrimSpace(it.BaseRef))
	d.Options = []StageTrackerDecisionOption{
		mergeNow,
		decisionOption(decisionActionCloseSuperseded, "Close as superseded", !held, decisionWhyHeld),
		decisionHoldOption(held),
	}
	return d
}
