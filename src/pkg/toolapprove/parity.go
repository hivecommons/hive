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
//   - a case in LegacyBaseDecision that maps the gate's answer onto a desk
//     verdict, computed from the SAME constant (SelfMergeMinACMMLevel) or the
//     SAME predicate (PlanPolicy, which the host wires to the config
//     functions the production gate calls) rather than a re-encoded threshold.
//
// parity_test.go (here, for the leaf-resident gates) and cmd/hive's
// approvalparity_test.go (for the plan gates behind PlanPolicy) then assert,
// for EVERY ACMM level 0..7, that the desk's verdict for a request permits
// exactly what the legacy gate permits. Because both sides read the same
// source, a future change to (say) SelfMergeMinACMMLevel moves both together
// and the tests keep passing — which is the point: they pin EQUIVALENCE, not
// a hardcoded table that would rot.

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/crux/config"
)

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
//
// plans answers the plan-approval kinds; nil fails closed to the operator lane
// (see PlanPolicy).
func LegacyBaseDecision(req Request, acmmLevel int, plans PlanPolicy) Verdict {
	if plans == nil {
		plans = PlanPolicyFuncs{}
	}
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
		if plans.PlanAutoApprove(acmmLevel) {
			v.Decision = DecisionAutoApprove
			v.Rationale = fmt.Sprintf("ACMM L%d pack sets plan_auto_approve: plan approved without operator", acmmLevel)
		} else {
			v.Decision = DecisionOperatorApprove
			v.Rationale = fmt.Sprintf("ACMM L%d pack does not set plan_auto_approve: plan requires operator approval", acmmLevel)
		}

	case KindPlanFromLabel:
		if plans.PlanFromLabel(acmmLevel) {
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
