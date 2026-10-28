package dashboard

import (
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Merge-gate states (StageTrackerMergeGate.State), in pipeline order. The
// Activity Monitor's Merge gate column lights one chevron for 060, 080,
// 095 and 100; the state names which, in words a row can show without a
// hover (hivecommons/hive#15073).
const (
	// MergeGateStatePending: 060 reviewed — the review swarm approved but
	// the admission gate still holds (CI pending or red, soak on an
	// unadmitted head, …); Reason says which.
	MergeGateStatePending = "gate-pending"
	// MergeGateStateReady: 080 awaiting merge admission — green and
	// approved, waiting for the automerge lane's next pass.
	MergeGateStateReady = "ready"
	// MergeGateStateSoaking: 095 pre-merge soak — admitted, the soak window
	// is running; SoakRemainingSeconds counts down.
	MergeGateStateSoaking = "soaking"
	// MergeGateStateMerging: 100 merge queue — admitted and soaked, the
	// merge is being attempted (GitHub merge queue / tide / merge window).
	MergeGateStateMerging = "merging"
	// MergeGateStateMerged: 110.
	MergeGateStateMerged = "merged"
)

// StageTrackerMergeGate narrates a PR's position at the merge gate: the
// state, what holds it (gate-pending), whether hive's merge sweep is on
// it right now, when the lane last evaluated it and when its next
// periodic pass is due, and the soak countdown. nil off the gate.
type StageTrackerMergeGate struct {
	State string `json:"state"`
	// Reason is the lane's reason token for the current stage (what the
	// gate waits on at 060: check-pending, required-check-pending:<name>).
	Reason string `json:"reason,omitempty"`
	// SweepRunning is true while an automerge pass that evaluates this PR
	// is in flight.
	SweepRunning bool `json:"sweep_running,omitempty"`
	// LastSweepAt is when the lane last evaluated this PR (the stage
	// record's LastSeen).
	LastSweepAt *time.Time `json:"last_sweep_at,omitempty"`
	// NextSweepAt is when the next full pass is due. A GitHub event on the
	// PR (check run, review, push) triggers a targeted pass sooner.
	NextSweepAt *time.Time `json:"next_sweep_at,omitempty"`
	// SoakUntil / SoakRemainingSeconds: the soak window's end, at 095.
	SoakUntil            *time.Time `json:"soak_until,omitempty"`
	SoakRemainingSeconds int64      `json:"soak_remaining_seconds,omitempty"`
}

// stageTrackerMergeGateState maps a stage to its merge-gate state; "" off
// the gate.
func stageTrackerMergeGateState(stage stagelabels.Stage) string {
	switch stage {
	case stagelabels.Reviewed:
		return MergeGateStatePending
	case stagelabels.AwaitingMergeAdmission:
		return MergeGateStateReady
	case stagelabels.PreMergeSoak:
		return MergeGateStateSoaking
	case stagelabels.MergeQueue:
		return MergeGateStateMerging
	}
	return ""
}

// stageTrackerMergeGate builds the merge-gate narration for a PR at
// stage, or nil when the PR is not at the gate.
func stageTrackerMergeGate(repo string, number int, stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, sweep automerge.SweepState, now time.Time) *StageTrackerMergeGate {
	state := stageTrackerMergeGateState(stage)
	if state == "" {
		return nil
	}
	mg := &StageTrackerMergeGate{State: state}
	if hasRec {
		mg.Reason = rec.Reason
		if !rec.LastSeen.IsZero() {
			t := rec.LastSeen
			mg.LastSweepAt = &t
		}
		if left := stageTrackerSoakRemaining(rec, now); left > 0 {
			mg.SoakRemainingSeconds = int64(left / time.Second)
			until := now.Add(left)
			mg.SoakUntil = &until
		}
	}
	if sweep.OnPR(repo, number) {
		mg.SweepRunning = true
	} else if !sweep.NextFloorAt.IsZero() && sweep.NextFloorAt.After(now) {
		t := sweep.NextFloorAt
		mg.NextSweepAt = &t
	}
	return mg
}

// stageTrackerSweepState is the lanes' live state, or the test override.
func (s *Server) stageTrackerSweepState() automerge.SweepState {
	if s != nil && s.sweepState != nil {
		return s.sweepState()
	}
	return automerge.SweepSnapshot()
}
