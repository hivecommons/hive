package hub

import (
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// upgradeProgressInput is everything upgradeProgressFor needs, gathered by
// the caller so the derivation itself is pure and table-testable.
type upgradeProgressInput struct {
	Entry *RegistryEntry
	// AutoUpgrade / Mode / Channel / LastFired describe the hive's schedule;
	// zero values mean "manual hive".
	AutoUpgrade bool
	Mode        string
	Channel     string
	LastFired   string
	// HoldReason is the schedule gate's most recent reason for holding an
	// open window (SaaSHive.AutoUpgradeHoldReason), with its timestamp.
	HoldReason string
	HoldAt     time.Time
	History    []spoke.UpgradeAttempt
	Now        time.Time
}

// upgradeProgressFor derives the render-ready upgrade state machine for one
// hive from the hub's latch, the spoke's last heartbeat, the schedule and
// the persisted history. It is the single place both the hive card and the
// heartbeat policy read, so the two dashboards cannot disagree (#14856).
//
// Precedence, in flight first:
//
//	UpgradeFailed                        → missed (spoke-reported error)
//	Upgrading + UpgradeDrain             → draining (countdown, agents)
//	Upgrading + beat reports the target  → verifying
//	Upgrading + spoke reports upgrading  → pulling
//	Upgrading + no beat since instruction→ rolling
//	Upgrading (older build, silent)      → pulling, qualified
//	open window held by the gate         → delayed (hold reason)
//	last history row ended recently      → that row's outcome
//	daily/weekly schedule                → scheduled (next window)
//	instant / spoke-managed              → scheduled (next poll)
//	manual                               → scheduled, no window
func upgradeProgressFor(in upgradeProgressInput) *spoke.UpgradeProgress {
	e := in.Entry
	if e == nil {
		return nil
	}
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

	if e.UpgradeFailed {
		reason := e.UpgradeError
		if reason == "" {
			reason = "the spoke reported the upgrade failed"
		}
		return &spoke.UpgradeProgress{
			State:   spoke.UpgradeStateMissed,
			Target:  historyRef(e.UpgradeTarget),
			From:    historyRef(e.GitHash),
			Reason:  reason,
			Detail:  "upgrade failed: " + reason,
			EndedAt: rfc(e.UpgradeFailedAt),
		}
	}

	if e.Upgrading {
		p := &spoke.UpgradeProgress{
			Target:      historyRef(e.UpgradeTarget),
			From:        historyRef(e.GitHash),
			StartedAt:   rfc(e.UpgradeStartedAt),
			AgentsTotal: len(e.Agents),
		}
		lastBeat := parseHeartbeatTime(e.LastHeartbeat)
		switch {
		case e.UpgradeDrain != nil:
			p.State = spoke.UpgradeStateDraining
			turns := e.UpgradeDrain.Turns
			if len(turns) == 0 {
				turns = spoke.TurnsFromNames(e.UpgradeDrain.InFlight)
			}
			p.AgentsInTurn = turns
			// The countdown is anchored on the beat that carried the report:
			// the spoke's RemainingS was true at send time, not at render time.
			remaining := time.Duration(e.UpgradeDrain.RemainingS) * time.Second
			anchor := lastBeat
			if anchor.IsZero() {
				anchor = now
			}
			deadline := anchor.Add(remaining)
			p.DrainDeadline = rfc(deadline)
			if left := deadline.Sub(now); left > 0 {
				p.DrainRemainingS = int(left / time.Second)
			}
			p.Detail = "draining: " + spoke.FormatTurns(turns, p.AgentsTotal) +
				" — proceeds in " + spoke.FormatShortDuration(deadline.Sub(now))
		case !lastBeat.IsZero() && !e.UpgradeStartedAt.IsZero() &&
			lastBeat.After(e.UpgradeStartedAt) && sameCommit(e.GitHash, e.UpgradeTarget):
			p.State = spoke.UpgradeStateVerifying
			p.Detail = "verifying: spoke is back on " + orDash(p.Target)
		case e.SpokeReportedUpgrading:
			p.State = spoke.UpgradeStatePulling
			p.Detail = "pulling: spoke acknowledged the upgrade to " + orDash(p.Target)
			if e.UpgradeRestartFallback != "" {
				p.Reason = e.UpgradeRestartFallback
			}
		case !e.UpgradeStartedAt.IsZero() && (lastBeat.IsZero() || !lastBeat.After(e.UpgradeStartedAt)):
			p.State = spoke.UpgradeStateRolling
			p.Detail = "rolling: no heartbeat since the instruction — pod is restarting onto " + orDash(p.Target)
		default:
			p.State = spoke.UpgradeStatePulling
			p.Detail = "instruction delivered; an older build may be draining silently"
			p.Reason = "spoke has not acknowledged the upgrade"
		}
		return p
	}

	history := spoke.UpgradeHistoryForDisplay(in.History, time.Time{})
	last, hasLast := latestClosedAttempt(history)
	// A newer closed attempt supersedes a stale schedule hold.
	daily := false
	if in.AutoUpgrade {
		switch effectiveAutoUpgradeSchedule(in.Mode, in.Channel) {
		case AutoUpgradeModeDaily, AutoUpgradeModeWeekly:
			daily = true
		}
	}
	nextWindow := ""
	if daily {
		nextWindow = nextScheduledAutoUpgradeAt(in.Mode, in.LastFired, now)
	}
	if daily && in.HoldReason != "" && scheduledUpgradeWindowOpen(in.Mode, in.LastFired, now) &&
		(!hasLast || in.HoldAt.After(last.EndedAt)) {
		return &spoke.UpgradeProgress{
			State:        spoke.UpgradeStateDelayed,
			From:         historyRef(e.GitHash),
			NextWindowAt: nextWindow,
			Reason:       in.HoldReason,
			Detail:       "delayed: " + in.HoldReason,
			EndedAt:      rfc(in.HoldAt),
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
			p.Detail = "upgraded to " + orDash(last.Target) + " " + ago
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

	p := &spoke.UpgradeProgress{State: spoke.UpgradeStateScheduled, From: historyRef(e.GitHash), NextWindowAt: nextWindow}
	switch {
	case !in.AutoUpgrade:
		p.Detail = "manual upgrades only"
	case daily:
		p.Detail = "next window " + nextWindow
	default:
		p.Detail = "upgrades on the hub's next poll"
	}
	return p
}

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

// upgradeProgressForRow is the hive-card wiring: it pulls schedule state off
// the hub-owned SaaSHive record (nil for an unmanaged spoke) and the
// persisted history for the row.
func (s *HubServer) upgradeProgressForRow(entry *RegistryEntry, sh *SaaSHive, channel string, now time.Time) (*spoke.UpgradeProgress, []spoke.UpgradeAttempt) {
	in := upgradeProgressInput{Entry: entry, Channel: channel, Now: now}
	if sh != nil {
		in.AutoUpgrade = sh.AutoUpgrade
		in.Mode = sh.AutoUpgradeMode
		in.LastFired = sh.AutoUpgradeLastFired
		in.HoldReason = sh.AutoUpgradeHoldReason
		in.HoldAt = sh.AutoUpgradeHoldAt
	}
	if s != nil && s.upgradeHistory != nil {
		in.History = s.upgradeHistory.recent(entry.ID)
	}
	return upgradeProgressFor(in), in.History
}
