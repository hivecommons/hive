package dashboard

import (
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// StageTrackerMergeGate distinguishes admission, soak and merge execution.
// Clock fields are null until a lane has actually scheduled/completed a pass.
type StageTrackerMergeGate struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
	github.MergeSweepTimes
	SoakUntil            *time.Time `json:"soak_until"`
	SoakRemainingSeconds int64      `json:"soak_remaining_seconds"`
}

func stageTrackerMergeGate(it StageTrackerItem, clock github.MergeSweepTimes, now time.Time) *StageTrackerMergeGate {
	state := "gate-pending"
	switch it.Stage {
	case 60:
		state = "gate-pending"
	case 80:
		state = "ready"
	case 95:
		state = "soaking"
	case 100:
		state = "merging"
	case 110:
		state = "merged"
	}
	gate := &StageTrackerMergeGate{State: state, Reason: it.Reason, MergeSweepTimes: clock, SoakRemainingSeconds: it.SoakRemainingSeconds}
	if it.Stage == 95 && it.SoakRemainingSeconds > 0 {
		until := now.Add(time.Duration(it.SoakRemainingSeconds) * time.Second)
		gate.SoakUntil = &until
	}
	return gate
}

func (s *Server) applyMergeGates(items []StageTrackerItem, now time.Time) {
	for i := range items {
		it := &items[i]
		if it.Kind != "pr" {
			continue
		}
		clock := github.MergeSweepTimes{}
		if s.deps != nil && s.deps.GHClient != nil {
			clock = s.deps.GHClient.MergeSweepClock().Snapshot(it.Repo, it.Number)
		}
		it.MergeGate = stageTrackerMergeGate(*it, clock, now)
	}
}
