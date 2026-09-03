package convergence

import (
	"reflect"
	"testing"
)

// These tests are the #4251 guard rows for the generation-comparison seam.
// Every blocking assertion here fails if the generation check in Evaluate is
// removed or bypassed — that is the strict RED acceptance requirement.

func boundObs(desired, observed int) Observation {
	return Observation{
		Subject: Subject{Repo: "hivecommons/hive", Number: 42},
		Outcome: &OutcomeStatus{
			Key:                "default/hivecommons/hive@nightly-green",
			DesiredGeneration:  desired,
			ObservedGeneration: observed,
		},
	}
}

// A transition requiring an outcome whose desired generation is G+1 while the
// observation is from G is unknown/not admitted; evidence for G can never
// authorize G+1.
func TestEvaluate_OutcomeStaleGenerationNeverAdmits(t *testing.T) {
	obs := boundObs(2, 1)
	obs.Found = false // even the always-admitting lookup-miss path must not admit

	d := Evaluate(obs)
	if d.Admitted {
		t.Fatalf("stale observed generation admitted a transition: %+v", d)
	}
	if d.Reason != ReasonOutcomeGenerationStale {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonOutcomeGenerationStale)
	}
	ready, ok := d.Condition(ConditionReady)
	if !ok || ready.Status != ConditionUnknown {
		t.Fatalf("Ready = %+v, want Unknown (nothing is established, caller should re-observe)", ready)
	}
	gen, ok := d.Condition(ConditionOutcomeGenerationObserved)
	if !ok || gen.Status != ConditionFalse || gen.Reason != ReasonOutcomeGenerationStale {
		t.Fatalf("OutcomeGenerationObserved = %+v, want False/%s", gen, ReasonOutcomeGenerationStale)
	}
	if len(d.Blockers) != 1 || d.Blockers[0] != obs.Outcome.Key {
		t.Fatalf("blockers = %v, want the outcome key", d.Blockers)
	}
	// The stale observed generation stays visible; it is not erased or latched.
	if d.Outcome == nil || d.Outcome.ObservedGeneration != 1 || d.Outcome.DesiredGeneration != 2 {
		t.Fatalf("decision must echo the stale generations: %+v", d.Outcome)
	}
}

// A never-observed declaration (observedGeneration zero) is stale, not
// satisfied: absence of observation cannot authorize.
func TestEvaluate_OutcomeNeverObservedNeverAdmits(t *testing.T) {
	d := Evaluate(boundObs(1, 0))
	if d.Admitted {
		t.Fatalf("never-observed declaration admitted a transition: %+v", d)
	}
	if d.Reason != ReasonOutcomeGenerationStale {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonOutcomeGenerationStale)
	}
}

// A matched generation establishes ONLY that the declaration generation was
// observed. The ordinary rules still decide admission, no Converged or
// predicate condition appears, and an unsatisfied dependency still blocks.
func TestEvaluate_OutcomeMatchIsNotSatisfaction(t *testing.T) {
	obs := boundObs(3, 3)
	obs.Found = true
	obs.RecordID = "bead-1"
	obs.Dependencies = []Dependency{{ID: "dep-a", Status: ConditionFalse}}

	d := Evaluate(obs)
	if d.Admitted {
		t.Fatalf("generation match must not override an unsatisfied dependency: %+v", d)
	}
	if d.Reason != ReasonWaitingForDependency {
		t.Fatalf("reason = %q, want the ordinary dependency verdict", d.Reason)
	}
	gen, ok := d.Condition(ConditionOutcomeGenerationObserved)
	if !ok || gen.Status != ConditionTrue || gen.Reason != ReasonOutcomeGenerationMatch {
		t.Fatalf("OutcomeGenerationObserved = %+v, want True/%s", gen, ReasonOutcomeGenerationMatch)
	}
	for _, c := range d.Conditions {
		if c.Type == "Converged" || c.Type == "Progressing" {
			t.Fatalf("uncomputed condition %q emitted from a generation match", c.Type)
		}
	}
	if d.Outcome == nil || d.Outcome.ObservedGeneration != 3 {
		t.Fatalf("decision must echo the matched status: %+v", d.Outcome)
	}
}

// A matched generation on an otherwise-admissible candidate admits through
// the ordinary rules and records the match condition additively.
func TestEvaluate_OutcomeMatchPreservesOrdinaryAdmission(t *testing.T) {
	obs := boundObs(1, 1)
	d := Evaluate(obs)
	if !d.Admitted || d.Reason != ReasonNoDeclaredDependencies {
		t.Fatalf("matched outcome broke the ordinary lookup-miss admission: %+v", d)
	}
	if _, ok := d.Condition(ConditionOutcomeGenerationObserved); !ok {
		t.Fatalf("match condition missing from admitted decision: %+v", d.Conditions)
	}
}

// Positive control: a candidate with NO declared outcome flows through
// Evaluate with byte-identical decisions — legacy admission is unchanged.
func TestEvaluate_NoDeclaredOutcomeIsByteIdenticalLegacy(t *testing.T) {
	cases := []Observation{
		{Subject: Subject{Repo: "o/r", Number: 1}},
		{Subject: Subject{Repo: "o/r", Number: 2}, Found: true, RecordID: "b",
			Dependencies: []Dependency{{ID: "x", Status: ConditionFalse}}},
		{Subject: Subject{Repo: "o/r", Number: 3}, Degraded: true, DegradedReason: "StoreDown"},
		{Subject: Subject{Repo: "o/r", Number: 4}, Found: true, RecordID: "b2",
			Dependencies: []Dependency{{ID: "y", Status: ConditionTrue}}},
	}
	for _, obs := range cases {
		d := Evaluate(obs)
		if d.Outcome != nil {
			t.Fatalf("unbound candidate grew an outcome echo: %+v", d)
		}
		for _, c := range d.Conditions {
			if c.Type == ConditionOutcomeGenerationObserved {
				t.Fatalf("unbound candidate grew an outcome condition: %+v", c)
			}
		}
	}
	// Spot-check full structural equality for the plain admit path.
	got := Evaluate(cases[0])
	want := Decision{
		Admitted: true,
		Reason:   ReasonNoDeclaredDependencies,
		Conditions: []Condition{
			{Type: ConditionObserved, Status: ConditionFalse, Reason: ReasonNoIntentRecord,
				Message: "no dependency-bearing record exists for o/r#1"},
			{Type: ConditionReady, Status: ConditionTrue, Reason: ReasonNoDeclaredDependencies,
				Message: "admitted: candidate declares no dependencies"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy decision changed shape:\n got %+v\nwant %+v", got, want)
	}
}

// A degraded read still authorizes nothing, even when the observer thinks the
// generations match: rule 1 stays first, because a non-authoritative read
// cannot vouch for the generation it claims to have seen.
func TestEvaluate_DegradedWinsOverOutcomeMatch(t *testing.T) {
	obs := boundObs(2, 2)
	obs.Degraded = true
	obs.DegradedReason = "AuthorityUnavailable:ledger"

	d := Evaluate(obs)
	if d.Admitted {
		t.Fatalf("degraded observation admitted: %+v", d)
	}
	if d.Reason != "AuthorityUnavailable:ledger" {
		t.Fatalf("reason = %q, want the degraded reason", d.Reason)
	}
	if _, ok := d.Condition(ConditionOutcomeGenerationObserved); ok {
		t.Fatalf("degraded read must not vouch for a generation match: %+v", d.Conditions)
	}
}

// Duplicate/out-of-order evidence cannot latch: Evaluate is pure, so the same
// stale observation yields the same refusal every time, and a subsequent
// current observation yields the match — order of calls is irrelevant.
func TestEvaluate_OutcomeGenerationDoesNotLatch(t *testing.T) {
	stale := boundObs(2, 1)
	current := boundObs(2, 2)
	first := Evaluate(stale)
	Evaluate(current)
	second := Evaluate(stale)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("stale decision changed after an interleaved current observation:\n%+v\n%+v", first, second)
	}
	if second.Admitted {
		t.Fatalf("replayed stale evidence admitted")
	}
}
