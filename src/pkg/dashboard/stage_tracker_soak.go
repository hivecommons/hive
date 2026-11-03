package dashboard

import (
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerSoak is the soak clock a PR row carries while the lane holds
// its head for the soak window — admitted and soaking at 095, or a
// trust-blocked head sitting through the same window at 060/080. The SPA
// counts down to Until in place and, once past it, says the head is ready
// for the next sweep; a HeadSHA that differs from the row's head means a
// push restarted the soak and the lane has not re-read it yet.
type StageTrackerSoak struct {
	// StartedAt is when the head was first seen green; Until is StartedAt
	// plus the window.
	StartedAt time.Time `json:"started_at"`
	Until     time.Time `json:"until"`
	// WindowSeconds is the lane's soak window (soak_minutes) in seconds.
	WindowSeconds int64 `json:"window_s"`
	// HeadSHA is the head the soak was measured on.
	HeadSHA string `json:"head_sha,omitempty"`
}

// stageTrackerSoakStages are the stages a soak clock is shown at: the
// merge gate before the merge itself.
func stageTrackerSoakStage(stage stagelabels.Stage) bool {
	switch stage {
	case stagelabels.Reviewed, stagelabels.AwaitingMergeAdmission, stagelabels.PreMergeSoak:
		return true
	}
	return false
}

// stageTrackerSoakFrom builds the soak clock for a record. The record's
// own soak (NoteSoak) wins; a 095 record written before it existed falls
// back to the lane's `soaking(remaining=…)` reason aged by the time since
// the sweep, with window the configured soak when known. nil when the
// row is not soaking.
func stageTrackerSoakFrom(rec stagelabels.Record, hasRec bool, head string, window time.Duration, now time.Time) *StageTrackerSoak {
	if !hasRec || !stageTrackerSoakStage(rec.Stage) {
		return nil
	}
	if soakHead, startedAt, win, ok := rec.Soak(); ok {
		return &StageTrackerSoak{StartedAt: startedAt, Until: startedAt.Add(win), WindowSeconds: int64(win / time.Second), HeadSHA: soakHead}
	}
	if rec.Stage != stagelabels.PreMergeSoak {
		return nil
	}
	left, ok := soakRemainingFromReason(rec.Reason)
	if !ok {
		return nil
	}
	until := now.Add(left)
	if !rec.LastSeen.IsZero() {
		until = rec.LastSeen.Add(left)
	}
	if window <= 0 {
		window = left
	}
	return &StageTrackerSoak{StartedAt: until.Add(-window), Until: until, WindowSeconds: int64(window / time.Second), HeadSHA: head}
}

// stageTrackerSoakWindow is the configured contributor-lane soak window.
func (s *Server) stageTrackerSoakWindow() time.Duration {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return 0
	}
	return time.Duration(s.deps.Config.AutoMerge.ContributorLane.EffectiveSoakMinutes()) * time.Minute
}
