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

// silentUpgradeDeadline is the hard upper bound on how long a latched upgrade
// may sit with NO heartbeat since it was instructed before the sweep clears it
// anyway.
//
// The liveness evidence below deliberately refuses to clear a latch while the
// spoke is silent — a restarting pod is not heartbeating, and clearing there
// could cancel a live rollout. But silence has to stop being exculpatory at
// SOME point: a pod that died mid-upgrade (failed image pull, crash loop on
// the new build, suspended/deleted cluster) never comes back, never
// heartbeats, and under the evidence-only rule its row spun "Upgrading"
// FOREVER — the live wedge on the z-aiops cohort, four hives at 1.8–1.9h and
// counting. No real rollout takes an hour of total silence: the spoke's own
// self-upgrade path gives up in minutes and even a cold-node image pull beats
// this by a wide margin. Six times the timeout the hub itself calls excessive
// is long enough that this lane can never race a genuine upgrade, and short
// enough that a dead attempt is reported the same session instead of never.
const silentUpgradeDeadline = 6 * staleUpgradeTimeout

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
	// converged marks the clear as a COMPLETED upgrade rather than an abandoned
	// one: the spoke is demonstrably at (or ahead of) the target it was asked
	// for, so the rollout succeeded and only the latch was left behind.
	//
	// The sweep must treat the two differently. An abandoned attempt is re-armed
	// and counts against maxOrphanedUpgradeSweeps, because the hive is still on
	// its old build and the instruction has to be re-delivered. A converged one
	// must do NEITHER: re-arming would re-instruct an upgrade that has already
	// happened (rolling a healthy pod), and burning retry budget would let three
	// successful upgrades tip the hive into a permanent, false UpgradeFailed.
	converged bool
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
		// attempt is gone. Short of silentUpgradeDeadline we do not clear — an
		// offline hive is reported by the offline alert instead — but past it
		// the silence IS the evidence: nothing that was going to report back
		// still exists. See evaluateSilentUpgrade.
		return evaluateSilentUpgrade(ev, zeroStart, entry)
	}
	if !lastBeat.After(entry.UpgradeStartedAt) {
		// Silent since the upgrade was instructed. That is what a restart in
		// progress looks like, so it must not be cleared here — until the
		// silence outlives any restart that could possibly still be in flight.
		return evaluateSilentUpgrade(ev, zeroStart, entry)
	}

	// The spoke is alive, post-dates the instruction, and is already ON the
	// target — or AHEAD of it (a floating-tag re-pull lands on whatever CI last
	// published, which can surpass a stale target; only ancestry can prove
	// that). The upgrade CONVERGED. Clear the latch here.
	//
	// This used to `return ev` (orphaned=false) on the reasoning that "the flag
	// is merely lagging and the heartbeat path clears it". That deferral is the
	// z-mlz-manager wedge: it is only true when a heartbeat actually arrives
	// carrying a state TRANSITION the completion chain in server.go recognises.
	// When the spoke's entry already records the target SHA — because the hub
	// asked for X, the spoke rolled onto X (or onto a newer Y that the entry has
	// since recorded), and every later beat therefore reports the SAME hash the
	// entry already holds — the completion chain sees no change to act on, so it
	// never fires. Both sides then defer to each other and the "Upgrading"
	// spinner, its elapsed counter and the stale SHA it was armed with survive
	// forever. Live case: z-mlz-manager sat Upgrading=true with
	// GitHash == UpgradeTarget == 815d7e4 for 35m+ while its pod was ready and
	// healthy on v4-latest.
	//
	// Deciding it HERE is safe precisely because we have already proven all
	// three of: the flag is latched, orphanedUpgradeClearAfter has elapsed (or
	// the start clock is corrupt), and the spoke has heartbeated SINCE the
	// instruction. A genuine in-flight rollout fails the last test — a
	// restarting pod is not heartbeating — so this can never cancel a live
	// upgrade.
	//
	// converged is reported distinctly from the un-upgraded orphan below so the
	// timeline and logs do not accuse a hive that in fact upgraded successfully
	// of having "never completed".
	//
	// commitAtOrAheadOfTarget is cache-only (the sweep holds s.mu), so an
	// unresolved pair is treated as "behind" this cycle and re-evaluated next.
	if entry.UpgradeTarget != "" && (sameCommit(entry.GitHash, entry.UpgradeTarget) ||
		commitAtOrAheadOfTarget(entry.GitHash, entry.UpgradeTarget, nil)) {
		ev.orphaned = true
		ev.converged = true
		ev.reason = "spoke is running " + orDash(entry.GitHash) +
			", at or ahead of the requested target " + orDash(entry.UpgradeTarget) +
			" — upgrade completed but the latch was never cleared"
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

// evaluateSilentUpgrade decides the fate of a latched upgrade whose spoke has
// NOT been heard from since the instruction. Below silentUpgradeDeadline the
// silence is treated as a restart in flight and the latch is left alone (the
// pre-existing behaviour). Past the deadline the latch is orphaned with an
// honest reason: whatever was restarting is not coming back on its own, and an
// eternal "Upgrading" spinner is a lie the offline alert does not correct.
// zeroStart (a corrupt/lost clock with no heartbeat either) stays untouchable:
// with neither an elapsed nor a beat there is nothing to measure the silence
// against, and such an entry cannot even render a growing spinner.
func evaluateSilentUpgrade(ev upgradeAttemptEvidence, zeroStart bool, entry *RegistryEntry) upgradeAttemptEvidence {
	if zeroStart || ev.elapsed < silentUpgradeDeadline {
		return ev
	}
	ev.orphaned = true
	ev.reason = "spoke has been silent for the entire " + roundedDuration(ev.elapsed) +
		" since the upgrade was instructed — the pod likely died mid-upgrade " +
		"(failed image pull, crash loop on the new build, or an unreachable cluster) " +
		"and no attempt is left to wait for; still recorded on " + orDash(entry.GitHash)
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
// orphanedUpgradeSweepInterval throttles sweepOrphanedUpgrades. The sweep
// repairs stale upgrade latches and re-arms missed deliveries — corrective
// work on the scale of minutes, not a hot path — so the 2-min SHA poller must
// not pay a full registry evaluation every tick as the fleet grows. 9 minutes
// is deliberately co-prime with the sibling 10/15-minute lanes so the throttled
// sweeps drift apart instead of stacking onto the same poller tick.
const orphanedUpgradeSweepInterval = 9 * time.Minute

// sweepOrphanedUpgradesIfDue runs the orphaned-upgrade sweep only if at least
// orphanedUpgradeSweepInterval has elapsed since the last run. Safe to call
// from the poller loop every tick; same throttle pattern and same guarding
// mutex as reconcileNetAdminIfDue.
func (s *HubServer) sweepOrphanedUpgradesIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastOrphanedUpgradeSweep.IsZero() ||
		time.Since(s.lastOrphanedUpgradeSweep) >= orphanedUpgradeSweepInterval
	if due {
		s.lastOrphanedUpgradeSweep = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.sweepOrphanedUpgrades()
}

func (s *HubServer) sweepOrphanedUpgrades() {
	// Admin kill switch: while spoke upgrades are paused nothing is being
	// delivered, so the ABANDONED-orphan half of this sweep must not run — it
	// re-arms heartbeatUpgrade (a delivery the pause forbids) and burns
	// OrphanedUpgradeSweeps retry budget against hives that CANNOT land their
	// target while paused, which would tip healthy hives into a false permanent
	// UpgradeFailed. Those latches simply wait; that half resumes with the rest
	// of the machinery.
	//
	// CONVERGED latches are different and are still cleared while paused:
	// clearing one delivers nothing (the upgrade already happened — the clear
	// even DROPS any armed instruction) and spends no retry budget, so the
	// pause has nothing to protect there. Skipping them made every hive whose
	// rollout finished right as an admin hit pause sit in an eternal
	// "Upgrading" spinner for the whole freeze — precisely the dishonest state
	// this sweep exists to remove.
	spokePauseSw, spokesArePaused := s.spokeUpgradesPaused()
	if spokesArePaused {
		s.logger.Debug("orphaned-upgrade sweep in paused mode — clearing completed latches only",
			"paused_by", spokePauseSw.By, "paused_at", spokePauseSw.At)
	}
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
		// converged marks a clear that completed successfully — logged and
		// timelined as a completion, never as an abandonment.
		converged bool
	}
	var swept []cleared

	s.mu.Lock()
	for i := range s.registry.Hives {
		h := &s.registry.Hives[i]
		// Resolve the image-verified latest for this hive's branch so the
		// evaluation can recognise a floating-tag hive that is already at latest
		// (and must not be swept/re-rolled toward a specific historical target).
		branch := s.upgradeBranchOrDefault(h.GitBranch)
		ev := evaluateOrphanedUpgrade(h, now, getLatestSHAForBranch(branch))
		if !ev.orphaned {
			continue
		}
		if ev.converged {
			// The upgrade LANDED; only the latch was stale. Clear it outright:
			// no retry-budget spend, no re-arm, and reset the counter so a hive
			// that needed one sweep on each of three separate upgrades is never
			// declared permanently faulty. Drop any armed instruction too, or
			// the next beat re-delivers an upgrade that already happened.
			swept = append(swept, cleared{
				id: h.ID, from: h.GitHash, to: h.UpgradeTarget,
				elapsed: ev.elapsed, reason: ev.reason, converged: true,
			})
			h.Upgrading = false
			h.UpgradeTarget = ""
			h.UpgradeStartedAt = time.Time{}
			h.OrphanedUpgradeSweeps = 0
			h.UpgradeFailed = false
			h.UpgradeError = ""
			h.UpgradeFailedAt = time.Time{}
			delete(s.heartbeatUpgrade, h.ID)
			continue
		}
		if spokesArePaused {
			// Abandoned orphan while paused: leave the latch, spend no budget,
			// re-arm nothing. See the pause rationale above the loop.
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
		//
		// Bounded by collectibility, like every other arming site: a hive that
		// cannot collect gains nothing from a re-arm here, and re-arming it
		// each sweep is one of the loops that kept the measured wedge alive.
		// This sweep is also where such a hive would otherwise spin forever —
		// evaluateOrphanedUpgrade() bails on an unparseable LastHeartbeat, so
		// the retry budget above is never spent and cannot retire it.
		if h.UpgradeTarget != "" && upgradeCollectible(h.LastHeartbeat, now) {
			s.heartbeatUpgrade[h.ID] = h.UpgradeTarget
		}
	}
	s.mu.Unlock()

	for _, c := range swept {
		if c.converged {
			s.logger.Info("cleared stale upgrade latch — upgrade had already completed",
				"hive_id", c.id,
				"elapsed", roundedDuration(c.elapsed),
				"sha", orDash(c.from),
				"target", orDash(c.to),
				"reason", c.reason,
			)
			s.recordTimeline(c.id, TimelineUpgradeStale,
				"upgrade to "+orDash(c.to)+" had already completed (hive is on "+
					orDash(c.from)+") — cleared a stale in-flight latch",
				"stale-upgrade-sweep")
			continue
		}
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
