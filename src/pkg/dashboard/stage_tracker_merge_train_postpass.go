package dashboard

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageTrackerMergeTrainFrontGate words the gate of the member merging
// first: "1st in the merge queue; merges next (1st of 2, overlapping
// files: a.go)". promoted is a member that was queued behind a front
// that just left: its queued words are stale and are replaced.
func stageTrackerMergeTrainFrontGate(front *StageTrackerItem, size int, files []string, promoted bool) {
	if front.MergeGate == nil {
		front.MergeGate = &StageTrackerMergeGate{}
	}
	if front.MergeGate.State == "" || front.MergeGate.State == MergeGateStateReady || (promoted && front.MergeGate.State == MergeGateStateQueuedBehind) {
		front.MergeGate.State = MergeGateStateTrainFront
	}
	if front.MergeGate.Reason == "" || promoted {
		front.MergeGate.Reason = "1st in the merge queue; merges next (1st of " + strconv.Itoa(size) + ", overlapping files: " + strings.Join(files, ", ") + ")"
	}
}

// stageTrackerMergeTrainPostPass runs over the finished items: the front
// (which the lane evaluates on its normal path and so carries no train
// reason) is marked from its siblings' records, and every member learns
// the PRs ahead of it. Members are matched on repo and base. A member
// under repair (stageTrackerTrainUnderRepair) leaves the train first —
// the front included — as does one short of the merge queue
// (stageTrackerBelowMergeQueue, which keeps an overlap hint instead);
// the rest close ranks (size, place, front and ahead recomputed), and a
// train left with one member is no train at all: no header, no ordinal
// (hivecommons/hive#15405 led a header as "1st: #15405" while its own
// chevron read "Fixing"; hive's own #15637/#15633 led one while both
// read CI / Review).
func stageTrackerMergeTrainPostPass(items []StageTrackerItem, opts mergeTrainQueueOptions, mem *mergeTrainMemory) {
	type trainKey struct{ repo, id string }
	// Every row the planner ordered, by train, before the view decides who
	// rides; the overlap hint on a row that does not names the rest.
	ordered := map[trainKey][]int{}
	samples := map[trainKey]*StageTrackerMergeTrain{}
	var keys []trainKey
	for i := range items {
		it := &items[i]
		if it.MergeTrain == nil || it.Kind != "pr" {
			continue
		}
		k := trainKey{it.Repo, it.MergeTrain.ID}
		if _, seen := samples[k]; !seen {
			samples[k] = it.MergeTrain
			keys = append(keys, k)
		}
		ordered[k] = append(ordered[k], i)
	}
	for _, key := range keys {
		sample := *samples[key]
		frontIdx := -1
		for i := range items {
			it := &items[i]
			if it.Kind == "pr" && it.Repo == key.repo && it.Number == sample.Front && it.Stage < stageTrackerMergedStage {
				frontIdx = i
				break
			}
		}
		roster := func(except int) []int {
			var out []int
			if frontIdx >= 0 && items[frontIdx].Number != except {
				out = append(out, items[frontIdx].Number)
			}
			for _, i := range ordered[key] {
				if items[i].Number != except {
					out = append(out, items[i].Number)
				}
			}
			sort.Ints(out)
			return out
		}
		var idx []int
		for _, i := range ordered[key] {
			it := &items[i]
			// A rebase-exhausted tail sits at 085 by the train's own doing;
			// it stays so the person who rebases it sees where it merges.
			if it.MergeTrain.State != MergeGateStateRebaseExhausted {
				if why := stageTrackerTrainUnderRepair(it.Stage, it.Reason); why != "" {
					stageTrackerLeaveMergeTrain(it, why)
					continue
				}
				if stageTrackerBelowMergeQueue(it.Stage) {
					stageTrackerLeaveMergeTrainBelowQueue(it, sample.Front, roster(it.Number))
					continue
				}
			}
			idx = append(idx, i)
		}
		frontLeft := false
		if frontIdx >= 0 && items[frontIdx].MergeTrain == nil {
			front := &items[frontIdx]
			if why := stageTrackerTrainUnderRepair(front.Stage, front.Reason); why != "" {
				front.LeftTrain = &StageTrackerLeftTrain{Why: why, Note: stageTrackerLeftTrainNote, trainID: sample.ID, position: 1}
				frontLeft = true
			} else if stageTrackerBelowMergeQueue(front.Stage) {
				front.Overlap = stageTrackerOverlapHint(front.Number, sample.Front, roster(front.Number), sample.OverlapFiles)
				front.Overlap.trainID, front.Overlap.position = sample.ID, 1
				frontLeft = true
			} else {
				front.MergeTrain = &StageTrackerMergeTrain{
					ID: sample.ID, State: MergeGateStateTrainFront, Front: sample.Front, Position: 1, Size: sample.Size,
					OverlapFiles: append([]string(nil), sample.OverlapFiles...),
				}
				stageTrackerMergeTrainFrontGate(front, sample.Size, sample.OverlapFiles, false)
				if front.Stage == stagelabels.AwaitingMergeAdmission.Number() {
					front.Caption = stageTrackerMergeTrainFrontCaption(&sample)
				}
				idx = append(idx, frontIdx)
			}
		}
		if len(idx) == 0 {
			continue
		}
		// Riders in planner order; the one seated first among them is the
		// front the lane merges first, and every member the view left
		// below the queue joins behind it when green — never ahead of it.
		sort.Slice(idx, func(a, b int) bool {
			return items[idx[a]].MergeTrain.Position < items[idx[b]].MergeTrain.Position
		})
		ridersFront := items[idx[0]].Number
		for i := range items {
			it := &items[i]
			if it.Kind == "pr" && it.Repo == key.repo && it.MergeTrain == nil && it.Overlap != nil && it.Overlap.trainID == sample.ID {
				stageTrackerOverlapJoinBehind(it, ridersFront)
			}
		}
		// The queue is the members that ride in this payload — never the
		// planner's count, which includes PRs the view does not seat
		// (below 080, under repair, a record another lane overwrote):
		// hive's own header read "2 of 9 PRs here · not in this view: 7
		// more" over the only two PRs actually queued (hivecommons/hive#15653).
		// Members the view sent elsewhere; one survivor is no train.
		var dropped []int
		for i := range items {
			it := &items[i]
			if it.Kind != "pr" || it.Repo != key.repo || it.MergeTrain != nil {
				continue
			}
			if it.LeftTrain != nil && it.LeftTrain.trainID == sample.ID {
				dropped = append(dropped, it.LeftTrain.position)
			} else if it.Overlap != nil && it.Overlap.trainID == sample.ID {
				dropped = append(dropped, it.Overlap.position)
			}
		}
		if len(dropped) > 0 {
			if sample.Size-len(dropped) < 2 {
				for _, i := range idx {
					stageTrackerLeaveMergeTrain(&items[i], "")
				}
				continue
			}
		}
		// Riders keep their planner order but take slots 1..N: a member
		// the planner seated 5th of 9 whose seven neighbours never rode
		// (below the merge queue, under repair, or not in this payload)
		// is 2nd of 2 here, never "5th of 2".
		sort.Slice(idx, func(a, b int) bool {
			return items[idx[a]].MergeTrain.Position < items[idx[b]].MergeTrain.Position
		})
		for n, i := range idx {
			items[i].MergeTrain.Position = n + 1
		}
		for _, i := range idx {
			mt := items[i].MergeTrain
			mt.Size = len(idx)
			// The front's gate and caption counted the planner's size;
			// say the size of the train that rides.
			if mt.State == MergeGateStateTrainFront && mt.Size != sample.Size {
				front := &items[i]
				stageTrackerMergeTrainFrontGate(front, mt.Size, mt.OverlapFiles, true)
				if front.Stage == stagelabels.AwaitingMergeAdmission.Number() && front.Caption == stageTrackerMergeTrainFrontCaption(&sample) {
					front.Caption = stageTrackerMergeTrainFrontCaption(mt)
				}
			}
		}
		// Members in merge order, so each one's Ahead is the prefix.
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
