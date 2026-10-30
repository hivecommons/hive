package automerge

import (
	"sync"
	"time"
)

// SweepState is what the automerge lanes are doing right now, for the
// dashboard's merge-gate rows (hivecommons/hive#15073): whether a pass is
// running and on which PRs, when the last pass finished and when the next
// periodic (floor) pass is due. Event-driven passes also run as webhooks
// name PRs, so NextFloorAt is the latest a waiting PR is re-evaluated, not
// the only time.
type SweepState struct {
	Running    bool
	Cause      string
	StartedAt  time.Time
	FinishedAt time.Time
	// NextFloorAt is when the next full pass is due; zero before the loop
	// has started.
	NextFloorAt time.Time
	// FloorEvery is the cadence the floor pass was last armed with — the
	// dashboard's unit for "one sweep" when it estimates a merge-train
	// member's turn (hivecommons/hive#15395). Zero before the loop starts.
	FloorEvery time.Duration
	// Targets names the PRs the running pass evaluates; nil means every PR.
	Targets SweepTargets
	// EvalRepo/EvalNumber name the one PR a lane is evaluating right now
	// (hivecommons/hive#15808). Once a pass has started walking PRs
	// (Narrowed), OnPR is true only for that PR: a floor pass runs every
	// lane serially over every PR and the dashboard must not read the
	// whole pass as "merging now" on every soaked row.
	EvalRepo   string
	EvalNumber int
	Narrowed   bool
}

// OnPR reports whether a pass is running now and evaluating repo#n.
func (s SweepState) OnPR(repo string, n int) bool {
	if !s.Running {
		return false
	}
	if s.Narrowed {
		return s.EvalNumber == n && s.EvalNumber > 0 &&
			canonicalSweepRepo(s.EvalRepo) == canonicalSweepRepo(repo)
	}
	if s.Targets == nil {
		return true
	}
	return s.Targets[canonicalSweepRepo(repo)][n]
}

// SweepClock records sweep passes for SweepState. One lives process-wide
// (SweepSnapshot) so the dashboard, which never holds the engine, can read
// the lanes' cadence.
type SweepClock struct {
	mu sync.Mutex
	st SweepState
}

func (c *SweepClock) begin(cause string, targets SweepTargets, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.st.Running, c.st.Cause, c.st.StartedAt, c.st.Targets = true, cause, now, targets
	c.mu.Unlock()
}

func (c *SweepClock) end(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.st.Running, c.st.FinishedAt, c.st.Targets = false, now, nil
	c.st.EvalRepo, c.st.EvalNumber, c.st.Narrowed = "", 0, false
	c.mu.Unlock()
}

// evaluating marks repo#n as the PR a lane is deciding on right now; the
// pass stays narrowed to one PR at a time until end.
func (c *SweepClock) evaluating(repo string, n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.st.EvalRepo, c.st.EvalNumber, c.st.Narrowed = repo, n, true
	c.mu.Unlock()
}

// evaluated clears the current PR mark once a lane has decided on it.
func (c *SweepClock) evaluated() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.st.EvalRepo, c.st.EvalNumber = "", 0
	c.mu.Unlock()
}

func (c *SweepClock) armFloor(next time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.st.NextFloorAt = next
	c.mu.Unlock()
}

// armFloorEvery arms the next floor pass at now+every and records the
// cadence for the dashboard's estimates.
func (c *SweepClock) armFloorEvery(now time.Time, every time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.st.NextFloorAt, c.st.FloorEvery = now.Add(every), every
	c.mu.Unlock()
}

// Snapshot returns a copy of the current state.
func (c *SweepClock) Snapshot() SweepState {
	if c == nil {
		return SweepState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	if st.Targets != nil {
		cp := make(SweepTargets, len(st.Targets))
		for repo, nums := range st.Targets {
			m := make(map[int]bool, len(nums))
			for n, v := range nums {
				m[n] = v
			}
			cp[repo] = m
		}
		st.Targets = cp
	}
	return st
}

var defaultSweepClock = &SweepClock{}

// SweepSnapshot is the process-wide lanes' state (see SweepState).
func SweepSnapshot() SweepState { return defaultSweepClock.Snapshot() }
