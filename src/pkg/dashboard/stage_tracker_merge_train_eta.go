package dashboard

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

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
		parts = append(parts, "head soaking "+mergeTrainShortDuration(headLeft))
	} else if aheadN > 0 && headState == mergeTrainHeadMerging {
		parts = append(parts, "head merging")
	}
	if own > 0 {
		parts = append(parts, "own soak "+mergeTrainShortDuration(own))
	}
	if sweeping {
		parts = append(parts, "pass running")
	} else if tick > 0 {
		parts = append(parts, "next pass in "+mergeTrainShortDuration(tick))
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
			return mergeTrainHeadSoaking, "soaking, " + mergeTrainShortDuration(left) + " left", left, true
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
		return mergeTrainHeadSoaking, "soaking, " + mergeTrainShortDuration(left) + " left", left, true
	}
	var next time.Duration
	if mg != nil && mg.NextSweepAt != nil && mg.NextSweepAt.After(now) {
		next = mg.NextSweepAt.Sub(now)
	}
	return mergeTrainHeadReady, "approved and green; merges on hive's next pass", next, true
}

// mergeTrainShortDuration is "12m" / "1h 05m" / "45s" for the chip's words.
func mergeTrainShortDuration(d time.Duration) string {
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
		"repo", repo, "train", mt.ID, "head", mt.HeadNumber, "head_state", mt.HeadState, "head_reason", mt.HeadReason, "stalled_for", mergeTrainShortDuration(time.Duration(mt.StalledS)*time.Second), "size", mt.Size)
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
