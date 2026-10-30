package dashboard

import (
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// Provisional is true while this member's own stage is still short of
	// the merge gate (050 review pending / CI, 060, 070…): the planner has
	// ordered it, but the slot is a forecast, not a state. The SPA keeps
	// the stage's blocker as the headline and words the slot as a suffix
	// ("CI / Review · queued 1st once ready") so the chevron never
	// contradicts the caption (hive's own #15577 read "Merge queued · 1st"
	// over "check build-and-test still running").
	Provisional bool `json:"provisional,omitempty"`
}

// stageTrackerMergeTrainProvisional reports whether a train member at
// stage is holding its queue slot ahead of being merge-ready: anything
// below 080 awaiting merge admission.
func stageTrackerMergeTrainProvisional(stage int) bool {
	return stage > 0 && stage < stagelabels.AwaitingMergeAdmission.Number()
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
	// trainID and position remember the slot it held so the survivors
	// can close ranks (stageTrackerMergeTrainPostPass).
	trainID  string
	position int
}

// stageTrackerTrainUnderRepair names why a PR at stage, with reason, is
// not a train member right now — "" when it may ride. A PR being fixed
// (070), waiting on its author (071) or contributor (072), freshly
// adopted (074), parked on a human decision (085), or whose required
// checks are red is somebody's to repair first; the planner may still
// have ordered it, but the view never seats it (hivecommons/hive#15581
// admits 050/060 provisionally only while nothing is red).
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

// stageTrackerMergeTrainFrontGate words the gate of the member merging
// first: "1st in the merge queue; merges next (1st of 2, overlapping
// files: a.go)", or "merges first once it is ready" while its own stage
// is short of the gate. promoted is a member that was queued behind a
// front that just left: its queued words are stale and are replaced.
func stageTrackerMergeTrainFrontGate(front *StageTrackerItem, size int, files []string, promoted bool) {
	if front.MergeGate == nil {
		front.MergeGate = &StageTrackerMergeGate{}
	}
	if front.MergeGate.State == "" || front.MergeGate.State == MergeGateStateReady || (promoted && front.MergeGate.State == MergeGateStateQueuedBehind) {
		front.MergeGate.State = MergeGateStateTrainFront
	}
	if front.MergeGate.Reason == "" || promoted {
		next := "merges next"
		if stageTrackerMergeTrainProvisional(front.Stage) {
			next = "merges first once it is ready"
		}
		front.MergeGate.Reason = "1st in the merge queue; " + next + " (1st of " + strconv.Itoa(size) + ", overlapping files: " + strings.Join(files, ", ") + ")"
	}
}

// stageTrackerMergeTrainPostPass runs over the finished items: the front
// (which the lane evaluates on its normal path and so carries no train
// reason) is marked from its siblings' records, and every member learns
// the PRs ahead of it. Members are matched on repo and base. A member
// under repair (stageTrackerTrainUnderRepair) leaves the train first —
// the front included — the rest close ranks (size, place, front and
// ahead recomputed), and a train left with one member is no train at
// all: no header, no ordinal (hivecommons/hive#15405 led a header as
// "1st: #15405" while its own chevron read "Fixing").
func stageTrackerMergeTrainPostPass(items []StageTrackerItem, opts mergeTrainQueueOptions, mem *mergeTrainMemory) {
	type trainKey struct{ repo, id string }
	members := map[trainKey][]int{}
	for i := range items {
		it := &items[i]
		if it.MergeTrain == nil || it.Kind != "pr" {
			continue
		}
		// A rebase-exhausted tail sits at 085 by the train's own doing;
		// it stays so the person who rebases it sees where it merges.
		if it.MergeTrain.State != MergeGateStateRebaseExhausted {
			if why := stageTrackerTrainUnderRepair(it.Stage, it.Reason); why != "" {
				stageTrackerLeaveMergeTrain(it, why)
				continue
			}
		}
		members[trainKey{it.Repo, it.MergeTrain.ID}] = append(members[trainKey{it.Repo, it.MergeTrain.ID}], i)
	}
	for key, idx := range members {
		sample := items[idx[0]].MergeTrain
		frontIdx := -1
		frontLeft := false
		for i := range items {
			it := &items[i]
			if it.Kind == "pr" && it.Repo == key.repo && it.Number == sample.Front && it.Stage < stageTrackerMergedStage {
				if why := stageTrackerTrainUnderRepair(it.Stage, it.Reason); why != "" {
					it.LeftTrain = &StageTrackerLeftTrain{Why: why, Note: stageTrackerLeftTrainNote, trainID: sample.ID, position: 1}
					frontLeft = true
					break
				}
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
			stageTrackerMergeTrainFrontGate(front, sample.Size, sample.OverlapFiles, false)
			if front.Stage == stagelabels.AwaitingMergeAdmission.Number() {
				front.Caption = stageTrackerMergeTrainFrontCaption(sample)
			}
			idx = append(idx, frontIdx)
		}
		// The slots the view has emptied: survivors behind them move up.
		var dropped []int
		for i := range items {
			it := &items[i]
			if it.Kind == "pr" && it.Repo == key.repo && it.MergeTrain == nil && it.LeftTrain != nil && it.LeftTrain.trainID == sample.ID {
				dropped = append(dropped, it.LeftTrain.position)
			}
		}
		if len(dropped) > 0 {
			if sample.Size-len(dropped) < 2 {
				for _, i := range idx {
					stageTrackerLeaveMergeTrain(&items[i], "")
				}
				continue
			}
			for _, i := range idx {
				mt := items[i].MergeTrain
				moved := 0
				for _, p := range dropped {
					if p > 0 && p < mt.Position {
						moved++
					}
				}
				mt.Position -= moved
				mt.Size = max(sample.Size-len(dropped), len(idx))
			}
		}
		// Members in merge order, so each one's Ahead is the prefix.
		sort.Slice(idx, func(a, b int) bool {
			return items[idx[a]].MergeTrain.Position < items[idx[b]].MergeTrain.Position
		})
		if frontLeft {
			// The old front is out; the member now in slot 1 leads.
			newFront := 0
			if first := &items[idx[0]]; first.MergeTrain.Position == 1 {
				newFront = first.Number
				first.MergeTrain.State = MergeGateStateTrainFront
				stageTrackerMergeTrainFrontGate(first, first.MergeTrain.Size, first.MergeTrain.OverlapFiles, true)
				frontIdx = idx[0]
			}
			for _, i := range idx {
				items[i].MergeTrain.Front = newFront
			}
		}
		members := make([]int, 0, len(idx))
		for _, i := range idx {
			members = append(members, items[i].Number)
		}
		for n, i := range idx {
			var ahead []int
			for _, j := range idx[:n] {
				ahead = append(ahead, items[j].Number)
			}
			items[i].MergeTrain.Ahead = ahead
			items[i].MergeTrain.Members = append([]int(nil), members...)
			items[i].MergeTrain.Provisional = stageTrackerMergeTrainProvisional(items[i].Stage)
		}
		var head *StageTrackerItem
		if frontIdx >= 0 {
			head = &items[frontIdx]
		}
		for n, i := range idx {
			ahead := make([]*StageTrackerItem, 0, n)
			for _, j := range idx[:n] {
				ahead = append(ahead, &items[j])
			}
			stageTrackerMergeTrainQueueFacts(&items[i], ahead, head, opts, mem)
		}
	}
}

// stageTrackerMergeTrainQueueFacts fills the chip's queue facts on one
// member: what the head is doing, how long it has sat there, the estimate
// of this member's own turn and whether the train reads stalled. ahead is
// every member before this one in merge order (the head first, when it is
// in the view); mem remembers each member's first estimate so a turn that
// has overrun it twice over reads "running late".
func stageTrackerMergeTrainQueueFacts(self *StageTrackerItem, ahead []*StageTrackerItem, head *StageTrackerItem, opts mergeTrainQueueOptions, mem *mergeTrainMemory) {
	if self == nil || self.MergeTrain == nil {
		return
	}
	mt := self.MergeTrain
	mt.Length = mt.Size
	mt.HeadNumber = mt.Front
	state, reason, headLeft, known := mergeTrainHeadState(head, opts.Now)
	mt.HeadState, mt.HeadReason = state, reason
	if head != nil && head.StageSince != nil && !head.StageSince.IsZero() {
		since := *head.StageSince
		mt.HeadSince = &since
	}
	mt.EtaS, mt.EtaAt, mt.EtaBasis, mt.Late = 0, nil, "", false
	// Stalled: the head has sat in its stage past the warn window and is
	// not actually merging. A soaking head is on its clock, not stalled,
	// until that clock has itself overrun the window.
	if opts.StallWarn > 0 && mt.HeadSince != nil && state != mergeTrainHeadMerging && state != mergeTrainHeadUnknown {
		sat := opts.Now.Sub(*mt.HeadSince)
		if sat > opts.StallWarn && !(state == mergeTrainHeadSoaking && headLeft > 0) {
			mt.Stalled = true
			mt.StalledS = int64(sat / time.Second)
		}
	}
	// No estimate while the head (or this member) waits on a person or on
	// CI: the blocker is the word, not a clock.
	if !known || self.NeedsHuman || self.Hold != nil {
		if mem != nil {
			mem.forgetEta(self)
		}
		return
	}
	// The estimate (hivecommons/hive#15395): this member's own soak left,
	// plus each PR ahead — its remaining soak, or one sweep when it has
	// none — plus the wait for the next sweep tick.
	sweep := opts.Sweep
	if sweep <= 0 {
		sweep = mergeTrainDefaultSweepEvery
	}
	var eta time.Duration
	own := stageTrackerSoakLeftOf(self, opts.Now)
	eta += own
	for i, a := range ahead {
		left := stageTrackerSoakLeftOf(a, opts.Now)
		if i == 0 && a == head {
			left = headLeft
		}
		if left <= 0 {
			left = sweep
		}
		eta += left
	}
	tick := opts.NextSweepIn(self)
	eta += tick
	if eta < time.Second {
		eta = time.Second
	}
	mt.EtaS = int64(eta / time.Second)
	at := opts.Now.Add(eta)
	mt.EtaBasis = mergeTrainEtaBasis(len(ahead), state, headLeft, own, tick, self.MergeGate != nil && self.MergeGate.SweepRunning)
	if mem != nil {
		at, mt.Late = mem.etaAnchor(self, mt, at, opts.Now, sweep)
	}
	mt.EtaAt = &at
}

// mergeTrainDefaultSweepEvery stands in for the lanes' floor cadence
// before the sweep loop has armed it (automerge's floor interval).
const mergeTrainDefaultSweepEvery = 5 * time.Minute

// NextSweepIn is how long until the next sweep tick: the member's own
// next_sweep_at when the gate carries one, else the lanes' floor; 0 while
// a sweep runs or none is armed.
func (o mergeTrainQueueOptions) NextSweepIn(self *StageTrackerItem) time.Duration {
	if self != nil && self.MergeGate != nil {
		if self.MergeGate.SweepRunning {
			return 0
		}
		if self.MergeGate.NextSweepAt != nil && self.MergeGate.NextSweepAt.After(o.Now) {
			return self.MergeGate.NextSweepAt.Sub(o.Now)
		}
	}
	if !o.NextSweepAt.IsZero() && o.NextSweepAt.After(o.Now) {
		return o.NextSweepAt.Sub(o.Now)
	}
	return 0
}

// stageTrackerSoakLeftOf is how much of an item's own soak clock remains.
func stageTrackerSoakLeftOf(it *StageTrackerItem, now time.Time) time.Duration {
	if it == nil {
		return 0
	}
	if it.MergeGate != nil && it.MergeGate.SoakRemainingSeconds > 0 {
		return time.Duration(it.MergeGate.SoakRemainingSeconds) * time.Second
	}
	if it.Soak != nil && it.Soak.Until.After(now) {
		return it.Soak.Until.Sub(now)
	}
	return 0
}

// mergeTrainEtaBasis is the short plain-English reading of the estimate:
// "2 ahead · head soaking 12m · next pass in 40s".
func mergeTrainEtaBasis(aheadN int, headState string, headLeft, own, tick time.Duration, sweeping bool) string {
	var parts []string
	switch aheadN {
	case 0:
		parts = append(parts, "1st in queue")
	case 1:
		parts = append(parts, "1 ahead")
	default:
		parts = append(parts, strconv.Itoa(aheadN)+" ahead")
	}
	if aheadN > 0 && headState == mergeTrainHeadSoaking && headLeft > 0 {
		parts = append(parts, "head soaking "+shortDuration(headLeft))
	} else if aheadN > 0 && headState == mergeTrainHeadMerging {
		parts = append(parts, "head merging")
	}
	if own > 0 {
		parts = append(parts, "own soak "+shortDuration(own))
	}
	if sweeping {
		parts = append(parts, "pass running")
	} else if tick > 0 {
		parts = append(parts, "next pass in "+shortDuration(tick))
	}
	return strings.Join(parts, " · ")
}

// mergeTrainHeadState reads the head's current state, its plain-word
// reason, how long its own clock still runs (soak left, next sweep) and
// whether that clock is known at all (false on a person or CI).
func mergeTrainHeadState(head *StageTrackerItem, now time.Time) (state, reason string, remaining time.Duration, known bool) {
	if head == nil {
		return mergeTrainHeadUnknown, "the head of the train is not in this view", 0, false
	}
	mg := head.MergeGate
	if mg != nil && mg.State == MergeGateStateMerging {
		return mergeTrainHeadMerging, "merging now", 0, true
	}
	if mg != nil && mg.State == MergeGateStateMergeFailed {
		return mergeTrainHeadReady, "GitHub refused the last merge; hive retries on its next pass", 0, false
	}
	switch head.Stage {
	case trackerNumberReadyForHuman:
		why := "waiting for a maintainer"
		if head.HumanMergeReason != "" {
			why += " (" + strings.ReplaceAll(head.HumanMergeReason, "-", " ") + ")"
		}
		return mergeTrainHeadNeedsHuman, why, 0, false
	case trackerNumberNeedsDecision, trackerNumberIssueDecision:
		return mergeTrainHeadNeedsHuman, "waiting for a human decision", 0, false
	case stagelabels.MergeQueue.Number():
		return mergeTrainHeadMerging, "merging now", 0, true
	case trackerNumberPreMergeSoak:
		left := time.Duration(0)
		if mg != nil && mg.SoakRemainingSeconds > 0 {
			left = time.Duration(mg.SoakRemainingSeconds) * time.Second
		}
		if left <= 0 && head.Soak != nil && head.Soak.Until.After(now) {
			left = head.Soak.Until.Sub(now)
		}
		if left > 0 {
			return mergeTrainHeadSoaking, "soaking, " + shortDuration(left) + " left", left, true
		}
		return mergeTrainHeadSoaking, "soaked; hive merges it on its next pass", 0, true
	case trackerNumberReviewed, trackerNumberReviewPending:
		return mergeTrainHeadCI, "waiting for CI", 0, false
	case trackerNumberFixing, trackerNumberReturned, trackerNumberNeedsAuthor:
		return mergeTrainHeadFixing, "being fixed by its author", 0, false
	}
	if head.Hold != nil || head.NeedsHuman {
		return mergeTrainHeadNeedsHuman, "waiting for a maintainer", 0, false
	}
	if head.Soak != nil && head.Soak.Until.After(now) {
		left := head.Soak.Until.Sub(now)
		return mergeTrainHeadSoaking, "soaking, " + shortDuration(left) + " left", left, true
	}
	var next time.Duration
	if mg != nil && mg.NextSweepAt != nil && mg.NextSweepAt.After(now) {
		next = mg.NextSweepAt.Sub(now)
	}
	return mergeTrainHeadReady, "approved and green; merges on hive's next pass", next, true
}

// shortDuration is "12m" / "1h 05m" / "45s" for the chip's words.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	m := int(d / time.Minute)
	if m < 60 {
		return strconv.Itoa(m) + "m"
	}
	rem := m % 60
	if rem == 0 {
		return strconv.Itoa(m/60) + "h"
	}
	rest := strconv.Itoa(rem)
	if rem < 10 {
		rest = "0" + rest
	}
	return strconv.Itoa(m/60) + "h " + rest + "m"
}

// mergeTrainMemory is what the tracker keeps between builds for the
// trains: the hour-stamp of each stalled train's activity-log line and
// each member's first estimate (for "running late").
type mergeTrainMemory struct {
	mu   sync.Mutex
	last map[string]time.Time
	eta  map[string]mergeTrainEtaMark
}

// mergeTrainEtaMark is a member's first estimate at a given position.
type mergeTrainEtaMark struct {
	At       time.Time
	Eta      time.Duration
	Position int
}

const mergeTrainStallLogEvery = time.Hour

// mergeTrainLateFactor: a turn still pending this many times past its
// first estimate reads "running late".
const mergeTrainLateFactor = 2

func mergeTrainMemberKey(it *StageTrackerItem) string {
	return it.Repo + "|" + strconv.Itoa(it.Number)
}

// etaAnchor keeps a member's countdown steady across builds: the first
// estimate at a position is the anchor the SPA counts down to, and a
// re-estimate only moves it when it runs more than one sweep longer (the
// head's clock grew) — otherwise every poll would push the end out by the
// sweep it just waited. It also reports "running late": now past the
// first estimate mergeTrainLateFactor times over.
func (m *mergeTrainMemory) etaAnchor(self *StageTrackerItem, mt *StageTrackerMergeTrain, fresh, now time.Time, sweep time.Duration) (time.Time, bool) {
	if m == nil || self == nil || mt == nil || mt.EtaS <= 0 {
		return fresh, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.eta == nil {
		m.eta = map[string]mergeTrainEtaMark{}
	}
	key := mergeTrainMemberKey(self)
	mark, ok := m.eta[key]
	if !ok || mark.Position != mt.Position || fresh.Sub(now) > mark.Eta+sweep {
		m.eta[key] = mergeTrainEtaMark{At: now, Eta: time.Duration(mt.EtaS) * time.Second, Position: mt.Position}
		return fresh, false
	}
	anchor := mark.At.Add(mark.Eta)
	if anchor.Before(now) {
		anchor = fresh
	}
	return anchor, now.Sub(mark.At) > mergeTrainLateFactor*mark.Eta
}

// forgetEta drops the member's mark: a blocked member starts a fresh
// estimate once it is unblocked.
func (m *mergeTrainMemory) forgetEta(self *StageTrackerItem) {
	if m == nil || self == nil {
		return
	}
	m.mu.Lock()
	delete(m.eta, mergeTrainMemberKey(self))
	m.mu.Unlock()
}

// warn logs the stalled train once per hour; it returns true when it logged.
func (m *mergeTrainMemory) warn(repo string, mt *StageTrackerMergeTrain, now time.Time) bool {
	if m == nil || mt == nil || !mt.Stalled {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string]time.Time{}
	}
	key := repo + "|" + mt.ID
	if at, ok := m.last[key]; ok && now.Sub(at) < mergeTrainStallLogEvery {
		return false
	}
	m.last[key] = now
	slog.Warn("merge train stalled: head has not progressed; "+strconv.Itoa(mt.Size-1)+" PR(s) wait behind it",
		"repo", repo, "train", mt.ID, "head", mt.HeadNumber, "head_state", mt.HeadState, "head_reason", mt.HeadReason, "stalled_for", shortDuration(time.Duration(mt.StalledS)*time.Second), "size", mt.Size)
	return true
}

// stageTrackerMergeTrainStallWarnings runs the hourly log over the items:
// one line per stalled train, read from any member.
func stageTrackerMergeTrainStallWarnings(mem *mergeTrainMemory, items []StageTrackerItem, now time.Time) {
	seen := map[string]bool{}
	for i := range items {
		mt := items[i].MergeTrain
		if mt == nil || !mt.Stalled {
			continue
		}
		key := items[i].Repo + "|" + mt.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		mem.warn(items[i].Repo, mt, now)
	}
}

// stageTrackerMergeTrainFrontCaption is the 080 caption of a train's front.
func stageTrackerMergeTrainFrontCaption(mt *StageTrackerMergeTrain) string {
	files := ""
	if len(mt.OverlapFiles) > 0 {
		files = " (overlapping files: " + strings.Join(mt.OverlapFiles, ", ") + ")"
	}
	return "1st in a merge queue of " + strconv.Itoa(mt.Size) + " PRs; merges next, then the lane updates or rebases the others" + files
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
