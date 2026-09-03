package proof

import (
	"github.com/hivecommons/hive/pkg/convergence"
)

// This file is the selective, non-monotonic invalidation judgment for
// kubestellar/hive#4254 (parent epic #3845). It is level-triggered by
// construction, exactly like convergence.Evaluate: the caller re-observes the
// CURRENT authoritative value of each declared input on every evaluation and
// the judgment is a pure function of (durable receipt, current inputs), so
// arrival order can never latch or regress truth, a duplicate or out-of-order
// input event changes nothing (the next evaluation re-reads current state),
// and a process restart reconstructs the identical invalidated/current status
// from the durable receipt alone. Historical work closure is not an input:
// nothing here consults a work record, so a closed task can never hold an
// outcome true after its evidence's assumptions moved.

// Verdict reasons added by selective invalidation. Stable machine-readable
// strings; callers log and test against them, so treat them as API.
const (
	// ReasonAssumptionInvalidated: the CURRENT authoritative value of an
	// input this receipt explicitly declares differs from the value its
	// judgment load-bears on. Only receipts declaring that assumption land
	// here; everything else is untouched.
	ReasonAssumptionInvalidated = "AssumptionInvalidated"
	// ReasonAssumptionUnknown: a declared input's current value could not be
	// observed authoritatively. Nothing is established (never satisfaction),
	// and per the accepted #4250 contract the effect is local to receipts
	// declaring that input — an unrelated proof remains available.
	ReasonAssumptionUnknown = "AssumptionUnknown"
)

// InputObservation is the current authoritative state of the declared input
// instances the caller could observe, keyed by scoped assumption name
// ("base-sha:owner/repo", "check-policy:owner/repo"). It is a snapshot, never
// a subscription: the caller re-observes on every evaluation.
type InputObservation struct {
	// Current maps an assumption name to its current authoritative value.
	Current map[string]string
	// Degraded maps an assumption name to a short reason when its observer
	// could not answer authoritatively (unavailable source, expired
	// freshness). A degraded input never invalidates and never confirms — it
	// makes ONLY the receipts declaring it Unknown.
	Degraded map[string]string
	// Observed marks input names the caller actually attempted. An input
	// absent from Observed is outside this snapshot entirely: the caller did
	// not look, so nothing about it can change any judgment. This keeps a
	// partial observation from silently confirming assumptions nobody
	// re-checked.
	Observed map[string]bool
}

// JudgeAssumptions judges this receipt's explicitly declared load-bearing
// inputs against the current input snapshot. It returns (verdict, true) when
// a declared assumption demotes the receipt — invalidated or unknown — and
// (zero, false) when every observed declared input still matches, in which
// case the ordinary fingerprint/freshness/result judgment stands.
//
// Selectivity is structural: the loop ranges over THIS receipt's declared
// assumptions only, so movement of an input the receipt never declared —
// another repository's base, a policy it does not rest on — cannot reach it.
// A definite mismatch is reported in preference to a degraded observer, the
// same "definite blocker first" ordering convergence.Evaluate applies.
func (r Record) JudgeAssumptions(inputs InputObservation) (Verdict, bool) {
	var degraded *Verdict
	for _, a := range r.declaredAssumptions() {
		if !inputs.Observed[a.Name] {
			continue
		}
		if reason, ok := inputs.Degraded[a.Name]; ok {
			if degraded == nil {
				degraded = &Verdict{Status: convergence.ConditionUnknown, Reason: ReasonAssumptionUnknown,
					Detail: "declared input " + a.Name + " could not be observed authoritatively: " + reason}
			}
			continue
		}
		if current, ok := inputs.Current[a.Name]; ok && current != a.Value {
			return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonAssumptionInvalidated,
				Detail: "declared input " + a.Name + " moved from " + a.Value + " to " + current +
					"; this judgment is no longer current"}, true
		}
	}
	if degraded != nil {
		return *degraded, true
	}
	return Verdict{}, false
}

// VerifyCurrent is the production judgment seam for an assumption-declaring
// proof: the ordinary exact-subject verification (missing, malformed,
// fingerprint mismatch, freshness, result) with selective invalidation
// applied to any receipt that would otherwise speak. A receipt whose declared
// input moved establishes NOTHING — not truth, not falsity — so its dependent
// transition is withheld through the existing evaluator rules (Unknown blocks
// as DependencyUnknown), while every receipt not declaring the moved input
// keeps the verdict Verify alone would give.
//
// Restoration is the same function: once a NEW receipt binding the new input
// value is accepted (a new fingerprint, a new key), its declared values match
// the current snapshot again and the predicate speaks — exactly that
// predicate, nothing else.
func VerifyCurrent(s *Store, ctx Context, inputs InputObservation) Verdict {
	if s == nil {
		return Verify(s, ctx)
	}
	rec, ok := s.Get(ctx.Required.Key())
	if !ok {
		return Verify(s, ctx)
	}
	if v, demoted := rec.JudgeAssumptions(inputs); demoted {
		return v
	}
	return rec.VerifyAgainst(ctx)
}

// DeclaringAssumption returns the keys of every stored receipt that declares
// the named input instance, sorted. It is the enumeration seam a status
// projection or an operator uses to answer "what does this input movement
// invalidate?" — and its complement proves selectivity: a receipt absent from
// this list is untouched by that input by construction.
func (s *Store) DeclaringAssumption(name string) []string {
	if name == "" {
		return nil
	}
	var out []string
	for _, rec := range s.List() {
		for _, a := range rec.declaredAssumptions() {
			if a.Name == name {
				out = append(out, rec.Key())
				break
			}
		}
	}
	return out
}
