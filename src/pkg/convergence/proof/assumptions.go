package proof

import (
	"fmt"
	"sort"
)

// This file adds the explicit non-monotonic input-assumption declaration for
// kubestellar/hive#4254 (parent epic #3845), consuming exactly the load-bearing
// inputs the accepted #4252 fingerprint selected for the first proof vertical
// (#4253): the bounded base-reference observation (BaseSHA) and the
// check-policy identity (CheckPolicyID).
//
// A declared assumption is the seam selective invalidation keys on: when the
// CURRENT authoritative value of a declared input differs from the value a
// receipt's judgment load-bears on, ONLY receipts declaring that exact
// assumption become non-current. A receipt that does not declare the moved
// assumption — a different repository's base, a different policy identity —
// remains valid, which is what keeps "main moved somewhere" from discarding
// unrelated evidence. Declarations are derived deterministically from the
// immutable fingerprint and persisted on the receipt, so the same
// invalidated/current status reconstructs from durable state after a restart:
// no verdict is ever cached, and historical closed work can never latch an
// outcome true.

// Assumption kinds. The v1 predicate load-bears on exactly these two inputs;
// a new predicate version declares its own set, never a silent extension of
// this one.
const (
	// AssumptionBaseSHA is the bounded base-reference observation input: the
	// PR base ref's commit SHA at observation time.
	AssumptionBaseSHA = "base-sha"
	// AssumptionCheckPolicy is the check-policy identity input: WHICH check
	// runs count, including the classifier ruleset revision.
	AssumptionCheckPolicy = "check-policy"
)

// assumptionSeparator joins an assumption kind to the repository scope it is
// observed in. ":" cannot appear in a canonical owner/repo spelling (Validate
// rejects whitespace and reserved separators, and a repo is exactly one "/"),
// so an assumption name is unambiguous by construction.
const assumptionSeparator = ":"

// Assumption is one explicitly declared load-bearing input of a proof
// judgment: the named input instance and the exact value the judgment rests
// on. Movement of the CURRENT authoritative value away from Value makes every
// receipt declaring this assumption non-current — and touches nothing else.
type Assumption struct {
	// Name identifies the input instance, scoped so unrelated instances can
	// never collide: "base-sha:owner/repo", "check-policy:owner/repo".
	Name string `json:"name"`
	// Value is the exact input value the judgment load-bears on.
	Value string `json:"value"`
}

// AssumptionName builds the scoped input-instance name for a kind observed in
// a repository.
func AssumptionName(kind, repo string) string {
	return kind + assumptionSeparator + repo
}

// DeclaredAssumptions derives the load-bearing input assumptions of the v1
// predicate from its immutable fingerprint: the base observation and the
// check-policy identity, both scoped to the subject repository. The
// derivation is deterministic, so durable receipts alone reconstruct the same
// declarations after a restart. Returns nil for a fingerprint that fails
// Validate — an unidentifiable subject declares nothing.
func (f Fingerprint) DeclaredAssumptions() []Assumption {
	if err := f.Validate(); err != nil {
		return nil
	}
	out := []Assumption{
		{Name: AssumptionName(AssumptionBaseSHA, f.Repo), Value: f.BaseSHA},
		{Name: AssumptionName(AssumptionCheckPolicy, f.Repo), Value: f.CheckPolicyID},
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// declaredAssumptions returns the record's effective declaration set: the
// persisted declarations when present, otherwise the deterministic derivation
// from the fingerprint. The fallback is the explicit additive-migration rule
// for receipts persisted before declarations existed: the v1 predicate has
// ALWAYS load-borne on both inputs, so a legacy receipt is treated as
// declaring exactly what its fingerprint proves it rested on — never as
// declaring nothing, which would exempt it from invalidation.
func (r Record) declaredAssumptions() []Assumption {
	if len(r.Assumptions) > 0 {
		return r.Assumptions
	}
	return r.Fingerprint.DeclaredAssumptions()
}

// validateAssumptions refuses a persisted declaration set that disagrees with
// the fingerprint the judgment actually rested on. A receipt claiming
// different assumptions than its own immutable evidence is malformed — it
// could otherwise dodge invalidation by under-declaring, or invalidate
// unrelated proofs by over-declaring.
func (r Record) validateAssumptions() error {
	if len(r.Assumptions) == 0 {
		return nil
	}
	want := r.Fingerprint.DeclaredAssumptions()
	if len(r.Assumptions) != len(want) {
		return fmt.Errorf("declared assumptions do not match the fingerprint's load-bearing inputs")
	}
	for i, a := range r.Assumptions {
		if a != want[i] {
			return fmt.Errorf("declared assumption %q disagrees with the fingerprint (%q)", a.Name, want[i].Name)
		}
	}
	return nil
}
