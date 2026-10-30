package dashboard

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github/automerge"
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
	// Members lists every member of the train in merge order (the front
	// first), so a view that carries only some of them can name the rest.
	Members []int `json:"members,omitempty"`
	// Queue facts for the chip (hivecommons/hive#15395): Length repeats
	// Size and HeadNumber repeats Front under the names the SPA's queue
	// wording uses; HeadState is what the head is doing now (one of the
	// mergeTrainHead* values) and HeadReason says it in plain words;
	// HeadSince is when the head entered its current stage; EtaS is the
	// estimated seconds until this member's own merge attempt (0 when no
	// estimate is possible — the head waits on a person or on CI), EtaAt
	// the same as a wall-clock instant for a countdown that survives
	// re-renders, EtaBasis its short reading ("2 ahead · next sweep in
	// 40s") and Late true once the turn has overrun its first estimate
	// twice over;
	// Stalled is true once the head has sat longer than
	// governor.merge_train_stall_warn_s, StalledS for how long.
	Length     int        `json:"length,omitempty"`
	HeadNumber int        `json:"head_number,omitempty"`
	HeadState  string     `json:"head_state,omitempty"`
	HeadReason string     `json:"head_reason,omitempty"`
	HeadSince  *time.Time `json:"head_since,omitempty"`
	EtaS       int64      `json:"eta_s,omitempty"`
	EtaAt      *time.Time `json:"eta_at,omitempty"`
	EtaBasis   string     `json:"eta_basis,omitempty"`
	Late       bool       `json:"late,omitempty"`
	Stalled    bool       `json:"stalled,omitempty"`
	StalledS   int64      `json:"stalled_s,omitempty"`
}

// stageTrackerBelowMergeQueue reports whether a PR at stage is short of
// the merge queue: anything below 080 awaiting merge admission. The
// merge train is a merge-queue concept — only PRs at 080 and beyond
// (green, approved, soaking or merging) ride one. A PR the planner has
// ordered while it is still at CI / Review is not a member: hive's own
// page led "merge train · 2 PRs · 1st: #15637" over #15637 and #15633
// while both chevrons read CI / Review (hivecommons/hive#15653; #15581 had
// admitted them provisionally).
func stageTrackerBelowMergeQueue(stage int) bool {
	return stage < stagelabels.AwaitingMergeAdmission.Number()
}

// Head states a merge train reports on its members (HeadState). The SPA
// words them; the API carries the token.
const (
	// mergeTrainHeadNeedsHuman: the head waits on a person (090 ready for
	// a human merge, 085 needs decision, a hold).
	mergeTrainHeadNeedsHuman = "needs-human"
	// mergeTrainHeadCI: the head's checks are still running or red (060).
	mergeTrainHeadCI = "ci"
	// mergeTrainHeadSoaking: the head is admitted and soaking (095, or a
	// soak clock at 080).
	mergeTrainHeadSoaking = "soaking"
	// mergeTrainHeadReady: approved and green, waiting for the next sweep.
	mergeTrainHeadReady = "ready"
	// mergeTrainHeadMerging: the merge call is in flight (100).
	mergeTrainHeadMerging = "merging"
	// mergeTrainHeadFixing: an agent is on the head (070/071/072).
	mergeTrainHeadFixing = "fixing"
	// mergeTrainHeadUnknown: the head is not in this payload (closed,
	// filtered, another page); the sibling only knows its number.
	mergeTrainHeadUnknown = "unknown"
)

// mergeTrainQueueOptions tunes the queue facts: Now for the clocks, Sweep
// for one sweep's worth of wait per PR ahead with no soak clock of its
// own (the lanes' floor cadence), NextSweepAt for the tick the estimate
// ends on, StallWarn for how long a head may sit before the train reads
// stalled.
type mergeTrainQueueOptions struct {
	Now         time.Time
	Sweep       time.Duration
	NextSweepAt time.Time
	StallWarn   time.Duration
}

// mergeTrainQueueOptionsFrom reads the options from the config and the
// lanes' live sweep clock: governor.merge_train_stall_warn_s is the stall
// threshold, the sweep's floor cadence the unit per PR ahead.
func mergeTrainQueueOptionsFrom(cfg *config.Config, sweep automerge.SweepState, now time.Time) mergeTrainQueueOptions {
	return mergeTrainQueueOptions{Now: now, Sweep: sweep.FloorEvery, NextSweepAt: sweep.NextFloorAt, StallWarn: cfg.MergeTrainStallWarn()}
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
	if mt.State == prstage.MergeTrainStateDuplicateOf {
		stageTrackerApplyDuplicateHold(it, mt)
		return
	}
	if mt.State == prstage.MergeTrainStateClaimConflict {
		stageTrackerApplyClaimConflict(it, mt)
		return
	}
	if mt.State == prstage.MergeTrainStateAdmitted {
		stageTrackerApplyAdmittedHold(it, mt)
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

// stageTrackerApplyDuplicateHold decorates a PR the sweep held out of the
// merge queue because another open PR fixes the same issue
// (hivecommons/hive#15395): the gate reads "duplicate-of:<winner>" with
// the winner and the issue on the gate, the row rides no train, and it
// wears needs-decision so "Let hive decide" offers the supersede path.
func stageTrackerApplyDuplicateHold(it *StageTrackerItem, mt *StageTrackerMergeTrain) {
	parsed, _ := prstage.ParseMergeTrain(it.Reason)
	if it.MergeGate == nil {
		it.MergeGate = &StageTrackerMergeGate{}
	}
	it.MergeGate.State = MergeGateStateDuplicateOf(mt.Front)
	it.MergeGate.Reason = parsed.Sentence()
	it.MergeGate.DuplicateOf = mt.Front
	it.MergeGate.DuplicateIssue = mt.Position
	it.MergeTrain = nil
	if !slices.Contains(it.Modifiers, modNeedsDecision) {
		it.Modifiers = append(it.Modifiers, modNeedsDecision)
	}
}

// stageTrackerApplyClaimConflict decorates a PR the sweep held because an
// open PR in another lane claims the same issue and nobody has decided
// which lands (hivecommons/hive#15810): the gate reads
// "claim-conflict:<other>" with the other PR and the issue on the gate,
// the row rides no train, the caption says "held · duplicate of #<other>
// · needs decision", and it wears needs-decision so the row offers the
// keep / close choice by name.
func stageTrackerApplyClaimConflict(it *StageTrackerItem, mt *StageTrackerMergeTrain) {
	parsed, _ := prstage.ParseMergeTrain(it.Reason)
	if it.MergeGate == nil {
		it.MergeGate = &StageTrackerMergeGate{}
	}
	it.MergeGate.State = MergeGateStateClaimConflict(mt.Front)
	it.MergeGate.Reason = parsed.Sentence()
	it.MergeGate.DuplicateOf = mt.Front
	it.MergeGate.DuplicateIssue = mt.Position
	it.MergeTrain = nil
	if !slices.Contains(it.Modifiers, modNeedsDecision) {
		it.Modifiers = append(it.Modifiers, modNeedsDecision)
	}
}

// stageTrackerClaimConflictCaption is the row caption of a PR held on an
// undecided same-issue claim, "" for every other row.
func stageTrackerClaimConflictCaption(it *StageTrackerItem) string {
	if it == nil || it.MergeGate == nil {
		return ""
	}
	other := mergeGateClaimConflictOf(it.MergeGate.State)
	if other <= 0 {
		return ""
	}
	return prstage.ClaimConflictCaption(other)
}

// stageTrackerApplyAdmittedHold decorates a PR the lane found green and
// approved while it shares files with a train (hivecommons/hive#15825):
// it holds no seat yet — the next plan seats it at the back — so the row
// rides no train (no ordinal, never the front) and carries the overlap
// hint instead, pointed at the front it joins behind.
func stageTrackerApplyAdmittedHold(it *StageTrackerItem, mt *StageTrackerMergeTrain) {
	parsed, _ := prstage.ParseMergeTrain(it.Reason)
	if it.MergeGate == nil {
		it.MergeGate = &StageTrackerMergeGate{}
	}
	it.MergeGate.State = MergeGateStateReady
	it.MergeGate.Reason = parsed.Sentence()
	it.MergeTrain = nil
	it.Overlap = stageTrackerOverlapHint(it.Number, mt.Front, nil, mt.OverlapFiles)
	it.Overlap.Note = "green and approved; takes a seat at the back of the merge train behind #" + strconv.Itoa(mt.Front) + " on the lane's next pass"
	it.Overlap.Badge = "train · joins behind #" + strconv.Itoa(mt.Front) + " next pass"
}

// Ejection captions (hivecommons/hive#15825): what the merge train's
// ejection means for this row, by the engine's why token. The chevron
// caption says what is owed; the gray pill carries the lineage.
var stageTrackerEjectionCaptions = map[string]string{
	"head-moved":   "left the merge train — new push, CI re-running after push; review re-run owed",
	"ci-pending":   "left the merge train — CI re-running on this head",
	"ci-red":       "left the merge train — a required check is red on this head",
	"verdict-owed": "left the merge train — review re-run owed for this head",
	"conflicting":  "left the merge train — conflicting with the base",
	"under-repair": "left the merge train — being fixed; rejoins when green",
	"human-merge":  "left the merge train — a person merges this one",
	"held":         "left the merge train — held",
}

// stageTrackerEjectionReasonPrefix prefixes the LeftTrain.Reason token the
// SPA derives its words from: `merge-train:ejected:<why>`.
const stageTrackerEjectionReasonPrefix = "merge-train:ejected:"

// stageTrackerApplyTrainEjection decorates a PR the merge train ejected
// (hivecommons/hive#15825) while it sits back in the CI / Review column:
// no merge_train, a left_train that names the why and the lineage pill
// ("left merge train · pushed 3m ago"), and a caption that says what is
// owed before it rides again. A row the view already seats (the record
// is stale) or one at 080+ keeps its own words.
func stageTrackerApplyTrainEjection(it *StageTrackerItem, rec stagelabels.Record, hasRec bool, now time.Time) {
	if it == nil || it.Kind != "pr" || !hasRec || it.MergeTrain != nil || it.Stage >= stagelabels.AwaitingMergeAdmission.Number() {
		return
	}
	why, head, at, ok := rec.TrainEjected()
	if !ok || (head != "" && it.Head != "" && !strings.EqualFold(head, it.Head) && why != "head-moved") {
		return
	}
	caption := stageTrackerEjectionCaptions[why]
	if caption == "" {
		caption = "left the merge train — " + why
	}
	lineage := "left merge train"
	switch {
	case why == "head-moved" && !rec.LastPushAt.IsZero():
		lineage += " · pushed " + shortDuration(now.Sub(rec.LastPushAt)) + " ago"
	case !at.IsZero():
		lineage += " · " + shortDuration(now.Sub(at)) + " ago"
	}
	it.LeftTrain = &StageTrackerLeftTrain{Why: why, Note: caption, Reason: stageTrackerEjectionReasonPrefix + why, Badge: lineage}
	it.Caption = caption
	if it.MergeGate != nil && it.MergeGate.State == MergeGateStateReady {
		it.MergeGate.State = ""
		if reflect.DeepEqual(*it.MergeGate, StageTrackerMergeGate{}) {
			it.MergeGate = nil
		}
	}
}

// stageTrackerLeftTrainNote is the muted hover note on a PR the view
// dropped from its merge train while a person or the fixer has it
// (hivecommons/hive#15405 led a train header as "1st: #15405" while its
// own chevron read "Fixing").
const stageTrackerLeftTrainNote = "left the merge train while being fixed; rejoins when green"

// stageTrackerAwaitingAdmissionCaption is the 080 row's own caption, off
// any train.
const stageTrackerAwaitingAdmissionCaption = "Green and approved; waiting for the automerge lane to admit it"

// StageTrackerLeftTrain says why a PR the planner had ordered rides no
// train in this view: Why is the repair token (fixing, needs-author,
// returned-to-contributor, needs-decision, checks-failing) and Note the
// hover sentence. The row has no merge_train and no ordinal.
type StageTrackerLeftTrain struct {
	Why  string `json:"why"`
	Note string `json:"note"`
	// Reason is the engine's ejection token (`merge-train:ejected:<why>`)
	// when the merge train itself ejected the PR (hivecommons/hive#15825);
	// Badge is the gray lineage pill ("left merge train · pushed 3m ago").
	Reason string `json:"reason,omitempty"`
	Badge  string `json:"badge,omitempty"`
	// trainID and position remember the slot it held so the survivors
	// can close ranks (stageTrackerMergeTrainPostPass).
	trainID  string
	position int
}
