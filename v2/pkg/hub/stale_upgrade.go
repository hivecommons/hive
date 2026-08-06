package hub

import (
	"time"
)

// orphanedUpgradeGrace is the EXTRA time, beyond staleUpgradeTimeout, that a
// hive must sit latched-upgrading before the sweep will clear its flag.
//
// It is deliberately derived from staleUpgradeTimeout rather than being an
// independent number, so retuning the one constant that already defines "this
// upgrade is stuck" (server.go) moves the warning threshold, the dashboard's
// red elapsed counter, the drift check and this sweep together.
//
// The clear threshold is longer than the warn threshold on purpose. At
// staleUpgradeTimeout the hub only says "this is taking too long" — a slow but
// real upgrade (cold-node image pull of a large layer) can still be in flight,
// and clearing there could cancel a legitimate rollout out from under itself.
// Doubling it means we only ever clear an attempt that has had twice the time
// the hub itself calls excessive, and only when the evidence below also says
// the attempt no longer exists.
const orphanedUpgradeGrace = staleUpgradeTimeout

// orphanedUpgradeClearAfter is the total elapsed time since UpgradeStartedAt
// after which an upgrade with no live attempt behind it is considered orphaned.
const orphanedUpgradeClearAfter = staleUpgradeTimeout + orphanedUpgradeGrace

// maxOrphanedUpgradeSweeps bounds how many times the sweep will clear and
// re-arm the same hive's upgrade before treating it as a genuine fault instead
// of a transient orphan.
//
// #2327's sweep assumes the attempt merely went missing, so re-arming lets the
// next heartbeat carry it again. That is right for a departed pod, but it is
// an infinite loop for a hive that is structurally unable to advance — the
// floating-tag case being the motivating example: the spoke restarts, re-pulls
// its mutable tag, lands on a build that is neither its old SHA nor the target,
// and reports in. The sweep sees "alive, not at target", clears, re-arms, and
// the cycle repeats indefinitely with nothing ever surfacing to a human.
//
// Three is deliberately small. Each sweep costs at least
// orphanedUpgradeClearAfter, so this is ~1 hour of real elapsed time at the
// default staleUpgradeTimeout before the hub gives up — long enough to absorb a
// genuinely slow or twice-unlucky rollout, short enough that a broken upgrade
// path is reported the same working day rather than never.
const maxOrphanedUpgradeSweeps = 3

// upgradeAttemptEvidence describes whether a still-latched upgrade has an
// attempt actually behind it, and if not, why we concluded that.
type upgradeAttemptEvidence struct {
	// orphaned is true when the flag should be cleared.
	orphaned bool
	// reason is a short human-readable justification, logged and surfaced.
	reason string
	// elapsed is the time since the upgrade was instructed.
	elapsed time.Duration
}

// evaluateOrphanedUpgrade decides whether entry's Upgrading flag is orphaned:
// set by a hub instruction whose spoke pod died before it could either complete
// the upgrade or report the failure, leaving nothing alive to clear it.
//
// The reliable signal is a heartbeat NEWER than UpgradeStartedAt that still
// reports the OLD SHA. During a genuine upgrade the spoke's pod is restarting
// into the new image, so it is not heartbeating; and the moment it comes back
// it reports the new GitHash, which the heartbeat path already uses to clear
// the flag (server.go). So a hive that is demonstrably alive and talking to us
// long after the upgrade was instructed, while still running the SHA it started
// on, cannot have an attempt in flight — the attempt is gone.
//
// Elapsed time alone is NOT sufficient and is not used alone here: a slow image
// pull looks identical to an orphan if you only look at the clock. Both
// conditions must hold.
// latestSHA is the image-verified latest short SHA for entry's branch (empty
// when unknown). It exists solely to recognise a FLOATING-TAG hive that has
// already converged: such a hive tracks a mutable …-latest tag, so a restart
// re-pulls the tag and it can only ever land on the newest build — never on the
// SPECIFIC historical commit the hub armed as UpgradeTarget. Judged by the
// exact-target test below, it would look orphaned on every sweep, get re-armed
// and re-rolled (restarting its pod) up to maxOrphanedUpgradeSweeps, then be
// wrongly reported as a permanent UpgradeFailed — while it was up to date the
// whole time. Passing latestSHA lets the evaluation see "floating tag already
// at latest" and treat it as converged, not orphaned.
func evaluateOrphanedUpgrade(entry *RegistryEntry, now time.Time, latestSHA string) upgradeAttemptEvidence {
	var ev upgradeAttemptEvidence

	if !entry.Upgrading {
		return ev
	}
	// A spoke that explicitly reported failure is already handled by the
	// heartbeat path, which clears Upgrading and records the cause. Leave that
	// terminal state alone — it carries a known reason this sweep would erase.
	if entry.UpgradeFailed {
		return ev
	}
	// Floating-tag hive already on the latest build for its branch: converged,
	// not orphaned. A mutable tag has no stable target commit, so the exact
	// UpgradeTarget test further down can never confirm it and would keep
	// sweeping/re-rolling it forever. The heartbeat completion path clears the
	// latch, so leave it alone here.
	if imageTagIsMutable(entry.ImageRef) && latestSHA != "" &&
		sameCommit(entry.GitHash, latestSHA) {
		return ev
	}
	// A zero UpgradeStartedAt with Upgrading still latched is not a fresh
	// upgrade — every path that sets Upgrading=true stamps UpgradeStartedAt to
	// time.Now() in the same breath (saas.go, saas_bulk.go), so a zero value can
	// only mean the timestamp was lost or reset while the flag survived. That is
	// the exact live wedge on ibm-alchemy: Upgrading=true, UpgradeStartedAt =
	// 0001-01-01, which the dashboard rendered as "Upgrading 17755944h28m" and
	// which no elapsed-based sweep can ever reach because there is no elapsed to
	// measure. We must not simply return here (the previous behaviour, which is
	// what let ibm-alchemy stay wedged forever); instead we skip the
	// elapsed-time gate and fall through to the SAME liveness evidence used for a
	// real stale upgrade. We still only clear when the spoke has demonstrably
	// checked in since — and is not already on the target — so a genuinely
	// fresh upgrade whose stamp merely lags a beat is never dropped.
	zeroStart := entry.UpgradeStartedAt.IsZero()
	if !zeroStart {
		ev.elapsed = now.Sub(entry.UpgradeStartedAt)
		if ev.elapsed < orphanedUpgradeClearAfter {
			return ev
		}
	}

	// Evidence: has the spoke checked in since we instructed the upgrade?
	lastBeat, err := time.Parse(time.RFC3339, entry.LastHeartbeat)
	if err != nil {
		// Never heartbeated, or an unparseable timestamp. We cannot prove the
		// attempt is gone, so we do not clear — an offline hive is reported by
		// the offline alert instead.
		return ev
	}
	if !lastBeat.After(entry.UpgradeStartedAt) {
		// Silent since the upgrade was instructed. That is what a restart in
		// progress looks like, so it must not be cleared here.
		return ev
	}

	// The spoke is alive and post-dates the instruction. If it is already on the
	// target — or AHEAD of it (a floating-tag re-pull lands on whatever CI last
	// published, which can surpass a stale target; only ancestry can prove
	// that) — the flag is merely lagging and the heartbeat path clears it; only
	// a spoke still on a SHA behind the target is genuinely un-upgraded.
	// commitAtOrAheadOfTarget is cache-only (the sweep holds s.mu), so an
	// unresolved pair is treated as "behind" this cycle and re-evaluated next.
	if entry.UpgradeTarget != "" && (sameCommit(entry.GitHash, entry.UpgradeTarget) ||
		commitAtOrAheadOfTarget(entry.GitHash, entry.UpgradeTarget, nil)) {
		return ev
	}

	ev.orphaned = true
	if zeroStart {
		// No trustworthy start time to report an elapsed from — the flag itself is
		// the corruption. Name that so the timeline reads honestly.
		ev.reason = "upgrade latched with no start time (lost/zero UpgradeStartedAt); spoke heartbeated " +
			roundedDuration(now.Sub(lastBeat)) + " ago still running " + orDash(entry.GitHash) +
			", no upgrade attempt in flight"
	} else {
		ev.reason = "spoke heartbeated " + roundedDuration(now.Sub(lastBeat)) +
			" ago still running " + orDash(entry.GitHash) +
			", no upgrade attempt in flight"
	}
	return ev
}

// sweepOrphanedUpgrades clears Upgrading flags left behind by spoke pods that
// vanished mid-upgrade. It runs on the hub, unconditioned on AutoUpgrade,
// because the flag is set by admin and bulk upgrade paths too — the existing
// recovery inside triggerAutoUpgrades() is gated on a hive having AutoUpgrade
// enabled and so never sees those hives at all.
//
// Clearing does NOT lose the fact that the upgrade never happened. The hive is
// still on its old SHA, and the sweep re-arms the heartbeat instruction so the
// next check-in carries the target again — the false "in flight" claim was
// SUPPRESSING delivery, since every upgrade path skips a hive it believes is
// mid-upgrade. UpgradeTarget is preserved for observability, and the event is
// logged and written to the hive's timeline.
func (s *HubServer) sweepOrphanedUpgrades() {
	now := time.Now()

	type cleared struct {
		id       string
		from, to string
		elapsed  time.Duration
		reason   string
		// exhausted marks a hive that has been swept too many times to keep
		// retrying — reported as a fault rather than re-armed.
		exhausted bool
		sweeps    int
	}
	var swept []cleared

	s.mu.Lock()
	for i := range s.registry.Hives {
		h := &s.registry.Hives[i]
		// Resolve the image-verified latest for this hive's branch so the
		// evaluation can recognise a floating-tag hive that is already at latest
		// (and must not be swept/re-rolled toward a specific historical target).
		branch := h.GitBranch
		if branch == "" {
			branch = "v2"
		}
		ev := evaluateOrphanedUpgrade(h, now, getLatestSHAForBranch(branch))
		if !ev.orphaned {
			continue
		}
		h.OrphanedUpgradeSweeps++
		exhausted := h.OrphanedUpgradeSweeps >= maxOrphanedUpgradeSweeps
		swept = append(swept, cleared{
			id:        h.ID,
			from:      h.GitHash,
			to:        h.UpgradeTarget,
			elapsed:   ev.elapsed,
			reason:    ev.reason,
			exhausted: exhausted,
			sweeps:    h.OrphanedUpgradeSweeps,
		})
		h.Upgrading = false
		// The orphaned attempt is being abandoned — drop its start clock so the
		// re-armed (or next) upgrade times from zero rather than inheriting this
		// dead attempt's elapsed. beginUpgrade will stamp a fresh start when the
		// recovery/heartbeat path re-enters.
		h.UpgradeStartedAt = time.Time{}

		if exhausted {
			// Stop retrying. Re-arming again would just reproduce the same
			// no-op on the next cycle; record a terminal, human-visible failure
			// instead, which the existing UpgradeFailed surfaces in the UI and
			// which AlertTypeStuckUpgrade already alerts on.
			h.UpgradeFailed = true
			h.UpgradeError = "upgrade to " + orDash(h.UpgradeTarget) +
				" never landed after " + itoa(h.OrphanedUpgradeSweeps) +
				" attempts — hive is still on " + orDash(h.GitHash) +
				". If its Deployment tracks a floating tag, the pod may be " +
				"re-pulling that tag and landing on a build other than the target."
			h.UpgradeFailedAt = now
			// Drop any armed instruction so no path keeps re-delivering it.
			delete(s.heartbeatUpgrade, h.ID)
			continue
		}

		// Re-arm delivery rather than dropping it. Clearing the flag alone is
		// NOT enough to make the hive upgrade again: heartbeatUpgrade is an
		// in-memory map (server.go), so a hub restart — the very event that
		// orphans these upgrades — wipes the armed target while the registry's
		// Upgrading=true survives on disk. The hive is left on the old SHA with
		// nothing instructing it.
		//
		// The two heartbeat re-instruct branches (server.go) fire only for a
		// spoke-managed hive or one with an armed hbTarget. A hub-managed hive
		// with AutoUpgrade off matches neither, so without this it would
		// silently stop receiving the upgrade. Re-arming the existing target
		// keeps the instruction alive; it is idempotent, and the heartbeat path
		// clears it as soon as the spoke reports the target SHA.
		if h.UpgradeTarget != "" {
			s.heartbeatUpgrade[h.ID] = h.UpgradeTarget
		}
	}
	s.mu.Unlock()

	for _, c := range swept {
		if c.exhausted {
			s.logger.Error("upgrade repeatedly failed to land — giving up and reporting a fault",
				"hive_id", c.id,
				"attempts", c.sweeps,
				"elapsed", roundedDuration(c.elapsed),
				"from", orDash(c.from),
				"to", orDash(c.to),
				"reason", c.reason,
				"hint", "check whether the spoke Deployment tracks a floating tag whose digest differs from the target SHA",
			)
			s.recordTimeline(c.id, TimelineUpgradeStale,
				"upgrade to "+orDash(c.to)+" abandoned after "+itoa(c.sweeps)+
					" attempts — hive is still on "+orDash(c.from)+
					" and is no longer being retried", "stale-upgrade-sweep")
			continue
		}
		s.logger.Warn("cleared orphaned upgrade flag — no attempt in flight",
			"hive_id", c.id,
			"attempt", c.sweeps,
			"elapsed", roundedDuration(c.elapsed),
			"from", orDash(c.from),
			"to", orDash(c.to),
			"reason", c.reason,
		)
		s.recordTimeline(c.id, TimelineUpgradeStale,
			"upgrade never completed after "+roundedDuration(c.elapsed)+
				" — cleared stale in-flight state (still on "+orDash(c.from)+
				", target was "+orDash(c.to)+")", "stale-upgrade-sweep")
	}
	if len(swept) > 0 {
		s.requestSave()
	}
}
