package toolapprove

// Migration parity for the four gates RFC #4000 inventoried.
//
// The binding constraint from the maintainer's throughput addendum: "Migration
// maps current behavior 1:1. Whatever a given ACMM level auto-permits today,
// the desk auto-permits on day one. New restrictions arrive only as explicit,
// audited policy changes — never as conservative shipped defaults that silently
// downgrade every high-autonomy hive on upgrade day."
//
// This file is how that constraint is kept honest. Each legacy gate gets:
//
//   - a LegacyGate* function that computes the gate's answer by calling the
//     SAME config constants and predicates the production gate calls, and
//   - a case in LegacyBaseDecision that maps that answer onto a desk verdict.
//
// parity_test.go then asserts, for EVERY ACMM level 0..7, that the desk's
// verdict for a request permits exactly what the legacy gate permits. Because
// the parity functions delegate to the real config predicates rather than
// re-encoding thresholds, a future change to (say) SelfMergeMinACMMLevel moves
// both sides together and the test keeps passing — which is the point: the test
// pins EQUIVALENCE, not a hardcoded table that would rot.

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/config"
)

// LegacyGateSelfMerge reports whether the App-self-merge sweep may merge a PR
// the App itself authored, at the given ACMM level.
//
// Delegates to config.AutoMergeConfig.SelfAuthoredAutoMergeAllowed — the same
// predicate SweepSelfAuthoredAutoMerges consults — so the desk cannot drift
// from the gate it is replacing. The gate history is recorded on
// SelfMergeMinACMMLevel: the sweep originally had NO ACMM check, which is how
// an L4 hive wrongly self-merged its own PR.
func LegacyGateSelfMerge(cfg config.AutoMergeConfig, acmmLevel int) bool {
	lvl := acmmLevel
	return cfg.SelfAuthoredAutoMergeAllowed(&lvl)
}

// LegacyGatePlanAutoApprove reports whether a decomposition plan auto-approves
// at the given level, delegating to config.PlanAutoApproveForLevel (the pack
// lookup the decomposition path uses).
func LegacyGatePlanAutoApprove(acmmLevel int) bool {
	return config.PlanAutoApproveForLevel(acmmLevel)
}

// LegacyGatePlanFromLabel reports whether the label planning trigger may fire
// at the given level, delegating to config.PlanningConfig.PlanFromLabelEnabled.
func LegacyGatePlanFromLabel(p config.PlanningConfig, acmmLevel int) bool {
	return p.PlanFromLabelEnabled(acmmLevel)
}

// LegacyGateQueuedMerge reports whether a queued merge may proceed. Unlike the
// other three this gate is identity-based rather than level-based: the sweep
// requires a trusted merger's queue approval (isTrustedMerger +
// latestHiveQueueApproval). The desk models that as "an approval that already
// happened", so trusted=true resolves and trusted=false goes to the operator
// lane rather than being denied outright.
func LegacyGateQueuedMerge(trustedApproval bool) bool {
	return trustedApproval
}

// LegacyBaseDecision produces the pre-rule, pre-ceiling verdict for the
// non-agent-tool request kinds, computed from the legacy gate predicates above
// so day-one behavior matches byte for byte.
//
// The ceiling in Desk.decide is applied on top of this, but note that for these
// kinds the base verdict is ALREADY derived from the level, so the ceiling is
// a no-op in the common case — it exists to bound operator rules, not to
// second-guess the legacy mapping.
func LegacyBaseDecision(req Request, acmmLevel int) Verdict {
	v := Verdict{
		Kind:      req.Kind,
		Tool:      req.Tool.Tool,
		ACMMLevel: acmmLevel,
		Agent:     req.Agent.Name,
	}

	if req.HasLegacyAllowed {
		if req.LegacyAllowed {
			v.Decision = DecisionAutoApprove
			v.Rationale = fmt.Sprintf("legacy %s gate allowed request; approval desk records and audits the explicit operation", req.Kind)
		} else {
			v.Decision = DecisionOperatorApprove
			v.Rationale = fmt.Sprintf("legacy %s gate did not allow request: routed to operator approval", req.Kind)
		}
		return v
	}

	switch req.Kind {
	case KindSelfMerge:
		// Parity note: the legacy gate ALSO requires AutoMergeConfig.Enabled and
		// the self-authored toggle. Those are carried by the caller's config and
		// checked at the call site before the request reaches the desk; the desk
		// reproduces only the LEVEL half of the gate, which is the half that was
		// bolted on after the L4 incident.
		if acmmLevel >= config.SelfMergeMinACMMLevel {
			v.Decision = DecisionAutoApprove
			v.Rationale = fmt.Sprintf(
				"ACMM L%d >= self-merge floor L%d: App-authored PR may self-merge",
				acmmLevel, config.SelfMergeMinACMMLevel)
		} else {
			v.Decision = DecisionOperatorApprove
			v.Rationale = fmt.Sprintf(
				"ACMM L%d is below the self-merge floor L%d: App-authored PR requires operator approval",
				acmmLevel, config.SelfMergeMinACMMLevel)
		}

	case KindPlanApproval:
		if LegacyGatePlanAutoApprove(acmmLevel) {
			v.Decision = DecisionAutoApprove
			v.Rationale = fmt.Sprintf("ACMM L%d pack sets plan_auto_approve: plan approved without operator", acmmLevel)
		} else {
			v.Decision = DecisionOperatorApprove
			v.Rationale = fmt.Sprintf("ACMM L%d pack does not set plan_auto_approve: plan requires operator approval", acmmLevel)
		}

	case KindPlanFromLabel:
		// PlanFromLabelEnabled takes a PlanningConfig; the zero value carries
		// the same defaults the config loader applies when the block is absent,
		// which is the behavior an unconfigured hive has today.
		if LegacyGatePlanFromLabel(config.PlanningConfig{}, acmmLevel) {
			v.Decision = DecisionAutoApprove
			v.Rationale = fmt.Sprintf("ACMM L%d permits the plan-from-label trigger", acmmLevel)
		} else {
			v.Decision = DecisionOperatorApprove
			v.Rationale = fmt.Sprintf("ACMM L%d does not permit the plan-from-label trigger without operator approval", acmmLevel)
		}

	case KindQueuedMerge:
		// A queued merge carries its own prior approval signal. The desk treats
		// a trusted-merger queue approval as satisfying the operator lane.
		v.Decision = DecisionOperatorApprove
		v.Rationale = "queued merge awaiting trusted-merger approval"

	default:
		// An unknown kind fails closed into the operator lane. A request the
		// desk does not recognize must never auto-resolve.
		v.Decision = DecisionOperatorApprove
		v.Rationale = fmt.Sprintf("unrecognized request kind %q: routed to operator approval (fail-closed)", req.Kind)
	}

	return v
}
