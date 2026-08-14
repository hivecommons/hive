package hub

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Automatic retirement of expired master generations, and the alert that fires
// when one is about to strand spokes (follow-on PR #7 of
// v2/docs/design/master-key-rotation.md).
//
// WHAT WAS ALREADY TRUE BEFORE THIS FILE, AND WHY THAT MATTERS. The security
// guarantee — step 5 of the rotation procedure, "after verify_until the
// previous generation stops being accepted, automatically, whether or not
// anyone is watching" — was ALREADY kept by acceptableGenerations(now) in
// hub_generations.go. That function excludes any non-current generation whose
// VerifyUntil has passed, and treats a ZERO VerifyUntil as ALREADY EXPIRED
// rather than as "never expires", so a hand-edited or malformed generations
// file fails closed. Every verifier on the platform goes through it. Nothing in
// this file weakens, duplicates, or re-implements that filter, and nothing in
// this file is load-bearing for the guarantee: if this whole lane never ran,
// an expired generation would still stop verifying on the wall clock.
//
// SO WHAT IS THIS FOR. Two things the read-path filter cannot do by itself:
//
//  1. PERSIST the drop. acceptableGenerations is a pure read-path predicate; it
//     leaves the dead entry sitting in the set on disk forever. A generation
//     that is no longer accepted but is still recorded is a plaintext master
//     secret retained on the hub PVC past the point where it protects anything
//     — the F1/F2 residue in a different form. Retirement rewrites
//     hub-generations.json without it, so the secret stops existing rather than
//     merely stopping being honoured.
//
//  2. ALERT. Retirement on the wall clock is unconditional, which means it can
//     and will strand spokes that have not converged. That is the CORRECT
//     behaviour — the alternative, waiting for convergence, is what lets one
//     unreachable spoke pin the old master open forever — but it is not a
//     silent one. The operator gets a warning as the window closes with spokes
//     still on the old key, and a louder one after it closes.
//
// THE TWO CONDITIONS ARE DELIBERATELY NOT THE SAME CONDITION. The design states
// both, and collapsing them would be the bug:
//
//   - RETIREMENT (here) is a WALL-CLOCK SECURITY GUARANTEE. It fires when
//     VerifyUntil has passed, FULL STOP — never gated on spokes_on_previous,
//     never gated on spokes_unattributed, never gated on whether anything was
//     observed at all. Gating it on convergence would mean a single spoke on an
//     unreachable cluster keeps a superseded master secret live indefinitely,
//     which is precisely the "unversioned and permanent compat lane" failure
//     mode the explicit-finiteness property exists to prevent.
//
//   - SafeToRetirePrevious (perhive_env_reconcile.go, unchanged by this file)
//     is an OPERATOR-FACING READINESS SIGNAL meaning "retiring right now costs
//     nothing". It fails closed on zero observations and on any unattributed
//     spoke, deliberately. It answers "is this free?", not "will this happen?".
//
// Retirement does not read SafeToRetirePrevious and must never learn to. The
// counts feed the ALERT only — they change how loudly retirement is announced,
// never whether it occurs.

const (
	// generationRetireInterval is how often the retirement sweep runs, matching
	// netAdminReconcileInterval and perHiveEnvReconcileInterval so the one
	// frequent SHA poller drives every periodic hub-internal lane on the same
	// cadence rather than acquiring a scheduler per lane.
	//
	// The interval bounds how long a dead entry LINGERS ON DISK, not how long
	// it is ACCEPTED — acceptance already ended on the wall clock at
	// VerifyUntil, via acceptableGenerations, with no reference to this value.
	// So a coarse interval costs cleanup latency, never acceptance.
	generationRetireInterval = 15 * time.Minute

	// generationPinWarnWindow is how long BEFORE VerifyUntil the pinned-open
	// alert starts firing when spokes are still on the outgoing generation.
	//
	// Sized to exceed one full reconcile sweep. The lane converges the fleet at
	// perHiveEnvMaxPatchesPerCycle per perHiveEnvReconcileInterval, so ~66
	// spokes take roughly 5.5 hours; 24 hours gives an operator time to notice
	// a stalled convergence and act (unpause a spoke, fix an unreachable
	// cluster) BEFORE the window closes on it, rather than reading about it
	// afterwards. Warning only at expiry would make the alert a post-mortem.
	generationPinWarnWindow = 24 * time.Hour
)

// generationPinSeverity ranks how urgent a pinned-open generation is.
type generationPinSeverity int

const (
	// pinSeverityNone means nothing to report: no expiring generation, or one
	// whose window is closing with no spoke left on it.
	pinSeverityNone generationPinSeverity = iota
	// pinSeverityWarn means the window is closing within
	// generationPinWarnWindow and spokes still carry the generation. Still
	// actionable — convergence can finish before expiry.
	pinSeverityWarn
	// pinSeverityStranded means the window has ALREADY closed while spokes
	// still carry the generation. Those spokes are now failing verification.
	// Not actionable by waiting; the operator must converge or re-provision
	// them.
	pinSeverityStranded
)

func (p generationPinSeverity) String() string {
	switch p {
	case pinSeverityWarn:
		return "warn"
	case pinSeverityStranded:
		return "stranded"
	default:
		return "none"
	}
}

// generationPinAlert is the pure, testable description of a pinned-open
// generation. It carries IDs, counts, and times — never key material.
type generationPinAlert struct {
	// Severity is what to do about it. pinSeverityNone means do not alert.
	Severity generationPinSeverity
	// GenerationID names the generation spokes are stuck on. An ID names a key;
	// it is not a key.
	GenerationID int
	// VerifyUntil is when that generation stops (or stopped) being accepted.
	VerifyUntil time.Time
	// SpokesOnPrevious is how many observed spokes carry material from a live
	// but non-current generation.
	SpokesOnPrevious int
	// SpokesUnattributed is how many observed spokes carry material matching NO
	// live generation.
	//
	// Counted into the alert exactly as PerHiveEnvSnapshot counts it into
	// SafeToRetirePrevious, and for the same reason: an unattributed spoke is
	// BROKEN NOW, not merely lagging, and must never be silently folded into
	// "converged". #3766 made it block retirement readiness deliberately;
	// letting it read as clean here would re-open that hole from the alerting
	// side.
	SpokesUnattributed int
	// ObservedHives is the denominator. Zero means the hub has read nothing, in
	// which case the counts above are evidence of nothing.
	ObservedHives int
}

// evaluateGenerationPin decides whether to alert about a generation whose
// verify window is closing while spokes still carry it.
//
// PURE. No clock, no locks, no I/O — `now` and the counts are arguments, so the
// policy is testable at any point on the timeline without sleeping.
//
// IT DOES NOT DECIDE WHETHER TO RETIRE. Retirement is the wall clock's call
// alone (see retireExpiredGenerations). This function only decides how loudly
// to say so. That separation is the whole point of the PR: a pinned-open
// generation produces an ALERT, never a stay of execution.
//
// FAILS CLOSED ON ZERO OBSERVATIONS, in the alerting direction. With
// observedHives == 0 the hub has read nothing, so "0 spokes on previous" is not
// evidence of convergence — but it is equally not evidence of stranding, and
// crying stranded from no data would train operators to ignore the alert. The
// readiness signal (SafeToRetirePrevious) is the surface that refuses to call
// zero observations safe; this one simply declines to claim knowledge it lacks.
func evaluateGenerationPin(
	gen keyGeneration,
	isCurrent bool,
	now time.Time,
	spokesOnPrevious int,
	spokesUnattributed int,
	observedHives int,
) generationPinAlert {
	alert := generationPinAlert{
		GenerationID:       gen.ID,
		VerifyUntil:        gen.VerifyUntil,
		SpokesOnPrevious:   spokesOnPrevious,
		SpokesUnattributed: spokesUnattributed,
		ObservedHives:      observedHives,
	}
	if isCurrent {
		// The current generation never expires and is never pinned open. It is
		// the thing spokes are converging TOWARD.
		return alert
	}
	// A spoke is "still carrying the old key" if it is on a previous generation
	// OR unattributed. Both are un-converged; only the second is also broken.
	stuck := spokesOnPrevious + spokesUnattributed
	if stuck == 0 || observedHives == 0 {
		return alert
	}
	// A ZERO VerifyUntil is ALREADY EXPIRED, matching acceptableGenerations
	// exactly. Reading it as "never expires" here would suppress the alert on
	// precisely the malformed file that most needs one.
	if gen.VerifyUntil.IsZero() || !now.Before(gen.VerifyUntil) {
		alert.Severity = pinSeverityStranded
		return alert
	}
	if !gen.VerifyUntil.After(now.Add(generationPinWarnWindow)) {
		alert.Severity = pinSeverityWarn
	}
	return alert
}

// retirableGenerations splits a set into the entries that survive retirement at
// `now` and the entries that must be dropped.
//
// PURE, and it does not mutate the receiver — the same discipline
// generationSet.rotate follows, so a caller that fails to persist has not
// already changed the hub's in-memory state.
//
// The keep/drop rule is acceptableGenerations' rule, applied to the STORED set
// rather than to a verification attempt: the current generation always stays,
// and a previous generation stays only while now is strictly before its
// VerifyUntil. A zero VerifyUntil drops. Deriving both from the same predicate
// is deliberate — if these two rules could drift, the hub could retire a
// generation it still accepts (breaking live artifacts) or keep one it does not
// (retaining a dead secret), and only the second failure would be visible.
func retirableGenerations(gs *generationSet, now time.Time) (keep []keyGeneration, drop []keyGeneration) {
	if gs == nil {
		return nil, nil
	}
	acceptable := make(map[int]bool, len(gs.Generations))
	for _, g := range gs.acceptableGenerations(now) {
		acceptable[g.ID] = true
	}
	for _, g := range gs.Generations {
		if g.ID == gs.Current || acceptable[g.ID] {
			keep = append(keep, g)
			continue
		}
		drop = append(drop, g)
	}
	return keep, drop
}

// errRetireWouldEmptySet guards against retiring the minting key.
var errRetireWouldEmptySet = errors.New("refusing to retire the current generation")

// retireExpiredGenerations drops every expired generation from the live set,
// persisting the result, and returns the IDs retired.
//
// UNCONDITIONAL ON CONVERGENCE, BY DESIGN. It never reads spoke counts and
// never consults SafeToRetirePrevious. A generation past VerifyUntil is retired
// even when every spoke in the fleet is still on it. This is the anti-pin
// property: making retirement wait for convergence would hand any single
// unconverged spoke — paused, unreachable, forgotten — the power to keep a
// superseded master secret on disk indefinitely, which is the exact failure the
// "explicitly finite" property exists to prevent. The stranding is instead made
// LOUD by the caller's alert, not prevented.
//
// PERSIST BEFORE INSTALL, matching rotateMasterSecret's ordering and for the
// same reason inverted: if the write fails, the hub keeps the old set in memory
// and returns an error, so the sweep retries next cycle. A retirement applied
// in memory but not on disk would come back at the next hub roll — several
// times a day — and an operator watching the surface would see a generation
// retire, vanish, and reappear. Persist-then-install makes a failed retirement
// a CLEAN NO-OP rather than a half-applied state.
//
// Returns (nil, nil) when there is nothing to retire, which is the overwhelming
// common case: the fleet is on one generation and no hub has ever rotated.
func (s *HubServer) retireExpiredGenerations(now time.Time) ([]int, error) {
	if s == nil {
		return nil, nil
	}
	// Hold the write lock across evaluate-persist-install for the reason
	// rotateMasterSecret documents: a rotation landing between our read and our
	// write would otherwise be silently discarded by our persist, dropping a
	// generation that is live material by then.
	s.keyGenerationsMu.Lock()
	defer s.keyGenerationsMu.Unlock()

	gs := s.keyGenerations
	keep, drop := retirableGenerations(gs, now)
	if len(drop) == 0 {
		return nil, nil
	}
	// keep always contains the current generation (retirableGenerations keeps
	// gs.Current unconditionally), so this cannot fire — but a set with no
	// minting key is unusable, not degraded, and installing one would silently
	// disable minting. Refuse rather than trust the invariant.
	if len(keep) == 0 {
		return nil, errRetireWouldEmptySet
	}
	next := newGenerationSet(gs.Current, keep)
	if next == nil {
		return nil, errRetireWouldEmptySet
	}

	retired := make([]int, 0, len(drop))
	for _, g := range drop {
		retired = append(retired, g.ID)
	}
	sort.Ints(retired)

	// PERSIST FIRST. lastKeyRotation is carried through unchanged: retirement
	// is not a rotation and must not reset the double-rotation cooldown, or an
	// operator could bypass the cooldown by waiting for a retirement instead of
	// for the fleet to converge.
	if err := saveGenerations(next, s.lastKeyRotation); err != nil {
		return nil, err
	}
	s.keyGenerations = next
	// AUDIT 8 / F19. Keep the spoke-bound derivation set in lockstep with the
	// minting set here too. Retirement does not change the CURRENT generation —
	// retirableGenerations always keeps gs.Current — so the six mandatory
	// per-hive vars are unaffected. What it does change is the set of PREVIOUS
	// generations, which is what provisionSSOPublicKeyPrevious and
	// provisionSessionPublicKeyPrevious read to emit the two _PREV public keys.
	// Without this line the sweep would keep advertising a _PREV public key for
	// a generation the hub has just stopped accepting, and the removal path in
	// perHiveEnvPatchJSON would never fire.
	setLiveGenerations(next, false)
	return retired, nil
}

// retireExpiredGenerationsIfDue runs the retirement sweep and the pinned-open
// alert, throttled to generationRetireInterval so the frequent SHA poller can
// call it every tick. Mirrors reconcileNetAdminIfDue and
// reconcilePerHiveEnvIfDue — this lane deliberately reuses the existing poller
// rather than adding a scheduler.
//
// THE THROTTLE IS A CLEANUP THROTTLE, NOT AN ACCEPTANCE THROTTLE. Acceptance
// already ends at VerifyUntil for every verifier via acceptableGenerations,
// independent of whether this ever runs. Skipping a tick delays only the
// rewrite of hub-generations.json and the alert.
func (s *HubServer) retireExpiredGenerationsIfDue() {
	if s == nil {
		return
	}
	s.clusterUnreachableMu.Lock()
	due := s.lastGenerationRetire.IsZero() ||
		time.Since(s.lastGenerationRetire) >= generationRetireInterval
	if due {
		s.lastGenerationRetire = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.sweepGenerationRetirement(time.Now())
}

// sweepGenerationRetirement alerts on pinned-open generations and then retires
// the expired ones.
//
// ORDER MATTERS: alert BEFORE retire. The alert describes generations that are
// still in the set, and the counts that make it meaningful are keyed to those
// IDs. Retiring first would drop the entry and leave the operator with a
// retirement notice and no way to see what it stranded.
func (s *HubServer) sweepGenerationRetirement(now time.Time) {
	if s == nil {
		return
	}
	gs := s.currentGenerations()
	if gs == nil {
		return
	}

	// Counts come from the Deployment-sourced snapshot, NOT from heartbeat
	// recency — the distinction PerHiveEnvSnapshot documents at length. A
	// paused spoke still has a Deployment, so it is still counted and still
	// shows up in this alert; it cannot fall out of the denominator by going
	// quiet and make a stranding look like a convergence.
	snap := s.PerHiveEnvSnapshot()

	// An UNREACHABLE spoke is a stranding candidate the pin alert cannot see:
	// evaluateGenerationPin reasons over observed counts, and an unread spoke
	// contributes to none of them. It is not merely absent from the alert — it
	// is the spoke most likely to be stranded, because nothing is converging
	// it. Retirement is unconditional on the wall clock, so say this plainly
	// and separately rather than letting a clean pin alert imply a clean fleet.
	if snap.UnreachableHives > 0 && s.logger != nil {
		s.logger.Warn("master generation retirement is running against a PARTIALLY OBSERVED fleet — "+
			"these spokes cannot be read, so their generation is unknown and the convergence lane "+
			"cannot repair them; retirement will not wait for them",
			"unreachable_hives", snap.UnreachableHives,
			"unreachable_clusters", strings.Join(snap.UnreachableClusters, ","),
			"observed_hives", snap.ObservedHives,
			"safe_to_retire_previous", snap.KeyGenerations.SafeToRetirePrevious)
	}

	for _, g := range gs.Generations {
		alert := evaluateGenerationPin(
			g,
			g.ID == gs.Current,
			now,
			snap.KeyGenerations.SpokesOnPrevious,
			snap.KeyGenerations.SpokesUnattributed,
			snap.ObservedHives,
		)
		if alert.Severity == pinSeverityNone {
			continue
		}
		if s.logger == nil {
			continue
		}
		// IDs, counts, and timestamps only. Never the secret, and never a
		// prefix or length of it either.
		args := []any{
			"generation", alert.GenerationID,
			"verify_until", alert.VerifyUntil.UTC().Format(time.RFC3339),
			"spokes_on_previous", alert.SpokesOnPrevious,
			"spokes_unattributed", alert.SpokesUnattributed,
			"observed_hives", alert.ObservedHives,
			"severity", alert.Severity.String(),
		}
		if alert.Severity == pinSeverityStranded {
			s.logger.Warn("master generation verify window has CLOSED while spokes still carry it — "+
				"those spokes are failing verification now; retirement is unconditional by design and "+
				"will not wait for them. Converge or re-provision them.", args...)
			continue
		}
		s.logger.Warn("master generation verify window is closing while spokes still carry it — "+
			"retirement is on the wall clock and will NOT wait for convergence. "+
			"Converge these spokes before verify_until or they will be stranded.", args...)
	}

	retired, err := s.retireExpiredGenerations(now)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("could not retire expired master generations; leaving the set unchanged and retrying next sweep",
				"path", hubGenerationsPath, "error", err)
		}
		return
	}
	if len(retired) > 0 && s.logger != nil {
		s.logger.Info("retired expired master generations",
			"retired", retired, "current", gs.Current, "path", hubGenerationsPath)
	}
}
