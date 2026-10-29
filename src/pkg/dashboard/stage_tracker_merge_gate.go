package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Merge-gate states (StageTrackerMergeGate.State), in pipeline order. The
// state names where a PR stands on its way to merge, in words a row can
// show without a hover (hivecommons/hive#15073). 060 (gate-pending) is
// pre-admission and is drawn in the CI / Review column as "Review passed ·
// <lane reason>" (CI in progress / checks unreadable / soak Nm remaining);
// the Merge gate column lights for 080, 095 and 100 (stageTrackerColumn).
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
	// MergeGateStateMergeFailed: a merge attempt the hive just made was
	// refused (hivecommons/hive#15245); MergeFailed says why. The PR is
	// still open at whatever stage the lane recorded.
	MergeGateStateMergeFailed = "merge-failed"
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
	// MergingSince is when the hive issued the merge call (state merging
	// from a live attempt, hivecommons/hive#15245); MergePath names the
	// caller (sweep, dashboard, relay, forge). The SPA holds the row on
	// "Merging" for at least the merge toast's lifetime from this instant.
	MergingSince *time.Time `json:"merging_since,omitempty"`
	MergePath    string     `json:"merge_path,omitempty"`
	// MergeFailed / MergeFailedAt: the refusal, in words, and when, for
	// state merge-failed.
	MergeFailed   string     `json:"merge_failed,omitempty"`
	MergeFailedAt *time.Time `json:"merge_failed_at,omitempty"`
	// MergeFailedKind / MergeFailedText / MergeFailedChecks: the refusal
	// translated (stageTrackerMergeRefusal) — a kind token the SPA maps to
	// a short chip detail, the plain sentence for the tooltip, and for
	// checks-pending how many required checks are still expected. The raw
	// MergeFailed text is for the tooltip's Details line only.
	MergeFailedKind   string `json:"merge_failed_kind,omitempty"`
	MergeFailedText   string `json:"merge_failed_text,omitempty"`
	MergeFailedChecks int    `json:"merge_failed_checks,omitempty"`
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

// stageTrackerMergeAttempts is the live merge-attempt registry keyed as
// MergeAttempts does (repo#number, lower-cased), or the test override.
func (s *Server) stageTrackerMergeAttempts() map[string]github.MergeAttempt {
	var list []github.MergeAttempt
	if s != nil && s.mergeAttempts != nil {
		list = s.mergeAttempts()
	} else {
		list = github.MergeAttemptsSnapshot()
	}
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]github.MergeAttempt, len(list))
	for _, a := range list {
		out[stageTrackerMergeAttemptKey(a.Repo, a.Number)] = a
	}
	return out
}

func stageTrackerMergeAttemptKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// stageTrackerMergeAttemptStage is the stage a PR row shows while the
// hive's merge call on it is in flight, or just returned merged and the
// PR is still listed open: 100, lit "Merging" (hivecommons/hive#15245).
// A refused attempt leaves the stage as recorded.
func stageTrackerMergeAttemptStage(stage stagelabels.Stage, a github.MergeAttempt, ok bool) stagelabels.Stage {
	if ok && (a.InFlight() || a.Merged) {
		return stagelabels.MergeQueue
	}
	return stage
}

// stageTrackerMergeGateAttempt folds a merge attempt into the gate
// narration: a live or just-successful attempt sets merging + since; a
// refused one sets merge-failed with the reason (even off the gate, so
// a 090 row whose maintainer merge GitHub refused says so). nil without
// an attempt.
func stageTrackerMergeGateAttempt(mg *StageTrackerMergeGate, a github.MergeAttempt, ok bool) *StageTrackerMergeGate {
	if !ok {
		return mg
	}
	if mg == nil {
		mg = &StageTrackerMergeGate{}
	}
	since := a.StartedAt
	switch {
	case a.InFlight() || a.Merged:
		mg.State = MergeGateStateMerging
		mg.MergingSince = &since
		mg.MergePath = a.Path
		mg.SoakUntil, mg.SoakRemainingSeconds, mg.NextSweepAt = nil, 0, nil
	case a.Failed():
		mg.State = MergeGateStateMergeFailed
		mg.MergingSince = &since
		mg.MergePath = a.Path
		mg.MergeFailed = a.Error
		mg.MergeFailedKind, mg.MergeFailedText, mg.MergeFailedChecks = stageTrackerMergeRefusal(a.Error)
		at := a.EndedAt
		mg.MergeFailedAt = &at
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

// stageTrackerMergingCaption is the row caption while the hive's merge
// call is in flight (or just returned merged): who issued it, in words.
func stageTrackerMergingCaption(a github.MergeAttempt) string {
	switch a.Path {
	case github.PRAuditPathDashboard:
		return "Merging now — a maintainer's merge from the dashboard is in flight"
	case github.PRAuditPathRelay:
		return "Merging now — the hive-merge relay's merge call is in flight"
	case github.PRAuditPathForge:
		return "Merging now — the forge adapter's merge call is in flight"
	}
	return "Merging now — the automerge sweep's merge call is in flight"
}
