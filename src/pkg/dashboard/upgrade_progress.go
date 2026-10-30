package dashboard

import (
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// upgradeDrainSource is the cmd/hive hook that exposes the live self-upgrade
// drain latch (selfUpgradeDrain.Report) to the dashboard, so /api/status
// can show the same countdown the hub sees on the heartbeat.
type upgradeDrainSource func(now time.Time) *spoke.UpgradeDrainReport

// SetUpgradeDrainSource installs the drain reporter. nil disables it.
func (s *Server) SetUpgradeDrainSource(fn func(now time.Time) *spoke.UpgradeDrainReport) {
	s.versionMu.Lock()
	s.upgradeDrain = fn
	s.versionMu.Unlock()
}

// upgradeProgressInput is what buildUpgradeProgress derives from, gathered
// at serve time so the derivation is pure and table-testable.
type upgradeProgressInput struct {
	Policy      *spoke.HeartbeatUpgradePolicy
	Drain       *spoke.UpgradeDrainReport
	Marker      map[string]any
	Outcome     *upgradeOutcome
	RunningSHA  string
	AgentsTotal int
	// StartedAt is process start — an outcome that completed on this boot
	// is the upgrade that just landed.
	StartedAt time.Time
	Now       time.Time
}

// buildUpgradeProgress derives the spoke's view of the upgrade state machine
// from what it knows locally — the drain latch, the self-upgrade marker, the
// last landed outcome — plus the hub's policy (next window, history). It
// uses the same state vocabulary as the hub card (#14856).
//
//	drain open                     → draining (countdown, agents in turn)
//	marker, attempts exhausted     → missed (last error)
//	marker, retrying               → pulling
//	outcome landed this boot       → done
//	hub history: recent row        → that row's outcome (missed window etc.)
//	policy with a schedule         → scheduled (next window / next poll)
//	no policy                      → nil (manual, nothing to show)
func buildUpgradeProgress(in upgradeProgressInput) *spoke.UpgradeProgress {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	rfc := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	nextWindow := ""
	var history []spoke.UpgradeAttempt
	if in.Policy != nil {
		nextWindow = in.Policy.NextWindowAt
		history = in.Policy.UpgradeHistory
	}
	var upgradedAt time.Time
	if in.Outcome != nil {
		upgradedAt = in.Outcome.CompletedAt
	}
	history = spoke.UpgradeHistoryForDisplay(history, upgradedAt)

	if d := in.Drain; d != nil {
		turns := d.Turns
		if len(turns) == 0 {
			turns = spoke.TurnsFromNames(d.InFlight)
		}
		remaining := time.Duration(d.RemainingS) * time.Second
		p := &spoke.UpgradeProgress{
			State:           spoke.UpgradeStateDraining,
			Target:          shortSHADashboard(d.Target),
			From:            shortSHADashboard(in.RunningSHA),
			StartedAt:       rfc(now.Add(-time.Duration(d.WaitedS) * time.Second)),
			DrainDeadline:   rfc(now.Add(remaining)),
			DrainRemainingS: d.RemainingS,
			AgentsInTurn:    turns,
			AgentsTotal:     in.AgentsTotal,
			NextWindowAt:    nextWindow,
		}
		p.Detail = "draining: " + spoke.FormatTurns(turns, in.AgentsTotal) +
			" — proceeds in " + spoke.FormatShortDuration(remaining)
		return p
	}

	if m := in.Marker; m != nil {
		target, _ := m["target"].(string)
		current, _ := m["current"].(string)
		requestedAt, _ := m["requestedAt"].(string)
		lastErr, _ := m["lastError"].(string)
		attempts, _ := m["attempts"].(int)
		maxAttempts, _ := m["maxAttempts"].(int)
		failed, _ := m["failed"].(bool)
		p := &spoke.UpgradeProgress{
			Target:       shortSHADashboard(target),
			From:         shortSHADashboard(current),
			StartedAt:    requestedAt,
			NextWindowAt: nextWindow,
			Reason:       lastErr,
		}
		if failed {
			p.State = spoke.UpgradeStateMissed
			if p.Reason == "" {
				p.Reason = "image pull or restart failed"
			}
			p.Detail = "upgrade failed after " + itoaDashboard(attempts) + " attempts: " + p.Reason
			return p
		}
		p.State = spoke.UpgradeStatePulling
		p.Detail = "pulling " + orDashDashboard(p.Target) + " (attempt " + itoaDashboard(attempts) + "/" + itoaDashboard(maxAttempts) + ")"
		return p
	}

	last, hasLast := latestClosedAttempt(history)
	if o := in.Outcome; o != nil && !o.CompletedAt.IsZero() && !in.StartedAt.IsZero() &&
		!o.CompletedAt.Before(in.StartedAt.Add(-time.Minute)) && (!hasLast || !last.EndedAt.After(o.CompletedAt)) {
		return &spoke.UpgradeProgress{
			State:        spoke.UpgradeStateDone,
			Target:       shortSHADashboard(o.TargetSHA),
			From:         shortSHADashboard(o.CurrentSHA),
			StartedAt:    rfc(o.RequestedAt),
			EndedAt:      rfc(o.CompletedAt),
			NextWindowAt: nextWindow,
			Detail:       "upgraded to " + orDashDashboard(shortSHADashboard(o.TargetSHA)) + " " + spoke.FormatShortDuration(now.Sub(o.CompletedAt)) + " ago",
		}
	}

	if hasLast && now.Sub(last.EndedAt) <= upgradeHistoryRecentFor {
		p := &spoke.UpgradeProgress{
			State:        last.Outcome,
			Target:       last.Target,
			From:         last.From,
			StartedAt:    rfc(last.StartedAt),
			EndedAt:      rfc(last.EndedAt),
			NextWindowAt: nextWindow,
			Reason:       last.Reason,
		}
		ago := spoke.FormatShortDuration(now.Sub(last.EndedAt)) + " ago"
		switch last.Outcome {
		case spoke.UpgradeOutcomeDone:
			p.Detail = "upgraded to " + orDashDashboard(last.Target) + " " + ago
		case spoke.UpgradeOutcomeDelayed:
			p.Detail = "delayed: " + last.Reason
		default:
			if last.Trigger == spoke.UpgradeTriggerWindow {
				p.Detail = "missed window " + ago + ": " + last.Reason
			} else {
				p.Detail = "missed " + ago + ": " + last.Reason
			}
		}
		return p
	}

	if in.Policy == nil || (!in.Policy.HubManaged && !in.Policy.SpokeManaged) {
		return nil
	}
	p := &spoke.UpgradeProgress{State: spoke.UpgradeStateScheduled, From: shortSHADashboard(in.RunningSHA), NextWindowAt: nextWindow}
	switch {
	case in.Policy.Paused:
		p.Detail = "upgrades paused"
	case nextWindow != "":
		p.Detail = "next window " + nextWindow
	default:
		p.Detail = "upgrades on the hub's next poll"
	}
	return p
}

// upgradeHistoryRecentFor mirrors the hub's window for showing the last
// attempt's outcome as the current state; after it the pill falls back to
// "scheduled".
const upgradeHistoryRecentFor = 24 * time.Hour

// latestClosedAttempt returns the most recently ended row of a history.
func latestClosedAttempt(history []spoke.UpgradeAttempt) (spoke.UpgradeAttempt, bool) {
	var best spoke.UpgradeAttempt
	found := false
	for _, a := range history {
		if a.Open() || a.EndedAt.IsZero() {
			continue
		}
		if !found || a.EndedAt.After(best.EndedAt) {
			best, found = a, true
		}
	}
	return best, found
}

// upgradeProgress gathers the live inputs and derives the state at serve
// time, so the drain countdown is never a cached eval cycle stale.
func (s *Server) upgradeProgress(status *StatusPayload, now time.Time) *spoke.UpgradeProgress {
	s.versionMu.RLock()
	policy := s.hubUpgradePolicy
	drainFn := s.upgradeDrain
	s.versionMu.RUnlock()
	in := upgradeProgressInput{
		Policy:    policy,
		Marker:    readUpgradeMarker(),
		Outcome:   readUpgradeOutcome(),
		StartedAt: s.startedAt,
		Now:       now,
	}
	if drainFn != nil {
		in.Drain = drainFn(now)
	}
	if status != nil {
		in.AgentsTotal = len(status.Agents)
	}
	if versionHash != "unknown" {
		in.RunningSHA = versionHash
	}
	return buildUpgradeProgress(in)
}

func itoaDashboard(n int) string {
	return strconv.Itoa(n)
}

func orDashDashboard(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
