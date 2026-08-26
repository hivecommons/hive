package convergence

import (
	"reflect"
	"testing"
)

// These tests pin the admission RULES themselves, independent of any observer.
// The production-path proof — that the same decision gates both ReadyQueue
// offerability and live selectTask assignment — lives in
// pkg/dashboard/contribute_dependency_admission_test.go.

func dep(id string, status ConditionStatus) Dependency {
	return Dependency{ID: id, Status: status}
}

func readyOf(t *testing.T, d Decision) Condition {
	t.Helper()
	c, ok := d.Condition(ConditionReady)
	if !ok {
		t.Fatalf("decision carries no %s condition: %+v", ConditionReady, d)
	}
	return c
}

func observedOf(t *testing.T, d Decision) Condition {
	t.Helper()
	c, ok := d.Condition(ConditionObserved)
	if !ok {
		t.Fatalf("decision carries no %s condition: %+v", ConditionObserved, d)
	}
	return c
}

// TestEvaluate_NoRecordAdmits is the explicit lookup-miss policy the design
// contract demands be stated rather than left accidental: a candidate with no
// dependency-bearing record has declared nothing to wait for, so it is ADMITTED
// — but Observed is recorded False, so the decision never claims an unknown
// dependency was satisfied.
func TestEvaluate_NoRecordAdmits(t *testing.T) {
	d := Evaluate(Observation{Subject: Subject{Repo: "acme/widget", Number: 7}})

	if !d.Admitted {
		t.Fatalf("a candidate with no record must be admitted, got %+v", d)
	}
	if got := observedOf(t, d); got.Status != ConditionFalse || got.Reason != ReasonNoIntentRecord {
		t.Fatalf("Observed should be False/%s, got %s/%s", ReasonNoIntentRecord, got.Status, got.Reason)
	}
	if got := readyOf(t, d); got.Status != ConditionTrue || got.Reason != ReasonNoDeclaredDependencies {
		t.Fatalf("Ready should be True/%s, got %s/%s", ReasonNoDeclaredDependencies, got.Status, got.Reason)
	}
	if len(d.Blockers) != 0 {
		t.Fatalf("no blockers expected, got %v", d.Blockers)
	}
}

// TestEvaluate_RecordWithNoDependenciesAdmits: an existing record that declares
// nothing is admitted, and — unlike the miss above — reports Observed=True.
// Distinguishing the two is the whole reason Observation.Found exists.
func TestEvaluate_RecordWithNoDependenciesAdmits(t *testing.T) {
	d := Evaluate(Observation{
		Subject: Subject{Repo: "acme/widget", Number: 7},
		Found:   true, RecordID: "bead-a", Generation: "g1",
	})

	if !d.Admitted {
		t.Fatalf("record with no dependencies must be admitted, got %+v", d)
	}
	if got := observedOf(t, d); got.Status != ConditionTrue {
		t.Fatalf("Observed should be True, got %s", got.Status)
	}
	if d.ObservedRecord != "bead-a" || d.ObservedGeneration != "g1" {
		t.Fatalf("decision must echo the observed record/generation, got %q/%q",
			d.ObservedRecord, d.ObservedGeneration)
	}
}

// TestEvaluate_UnsatisfiedDependencyBlocks is the core gate.
func TestEvaluate_UnsatisfiedDependencyBlocks(t *testing.T) {
	d := Evaluate(Observation{
		Subject: Subject{Repo: "acme/widget", Number: 7},
		Found:   true, RecordID: "bead-a",
		Dependencies: []Dependency{dep("bead-b", ConditionFalse), dep("bead-c", ConditionTrue)},
	})

	if d.Admitted {
		t.Fatalf("an unsatisfied dependency must block admission, got %+v", d)
	}
	if d.Reason != ReasonWaitingForDependency {
		t.Fatalf("reason should be %s, got %s", ReasonWaitingForDependency, d.Reason)
	}
	if got := readyOf(t, d); got.Status != ConditionFalse {
		t.Fatalf("Ready should be False (established, not Unknown), got %s", got.Status)
	}
	if !reflect.DeepEqual(d.Blockers, []string{"bead-b"}) {
		t.Fatalf("only the unsatisfied dependency should be a blocker, got %v", d.Blockers)
	}
}

// TestEvaluate_SatisfiedDependenciesAdmit: satisfaction is reversible in both
// directions — the same candidate admits once every dependency reads True.
func TestEvaluate_SatisfiedDependenciesAdmit(t *testing.T) {
	d := Evaluate(Observation{
		Subject: Subject{Repo: "acme/widget", Number: 7},
		Found:   true, RecordID: "bead-a",
		Dependencies: []Dependency{dep("bead-b", ConditionTrue), dep("bead-c", ConditionTrue)},
	})

	if !d.Admitted {
		t.Fatalf("all-satisfied dependencies must admit, got %+v", d)
	}
	if d.Reason != ReasonDependenciesSatisfied {
		t.Fatalf("reason should be %s, got %s", ReasonDependenciesSatisfied, d.Reason)
	}
}

// TestEvaluate_UnknownDependencyBlocksAsUnknown: an unresolvable dependency
// cannot be asserted satisfied, so the candidate is withheld — but as Unknown,
// not False. The distinction matters: False is an established blocker an
// operator can act on, Unknown is a gap to be closed by safe observation.
func TestEvaluate_UnknownDependencyBlocksAsUnknown(t *testing.T) {
	d := Evaluate(Observation{
		Subject: Subject{Repo: "acme/widget", Number: 7},
		Found:   true, RecordID: "bead-a",
		Dependencies: []Dependency{dep("bead-ghost", ConditionUnknown), dep("bead-c", ConditionTrue)},
	})

	if d.Admitted {
		t.Fatalf("an unresolvable dependency must not be waved through, got %+v", d)
	}
	if d.Reason != ReasonDependencyUnknown {
		t.Fatalf("reason should be %s, got %s", ReasonDependencyUnknown, d.Reason)
	}
	if got := readyOf(t, d); got.Status != ConditionUnknown {
		t.Fatalf("Ready should be Unknown, got %s", got.Status)
	}
	if !reflect.DeepEqual(d.Blockers, []string{"bead-ghost"}) {
		t.Fatalf("the unresolved dependency should be the blocker, got %v", d.Blockers)
	}
}

// TestEvaluate_EstablishedBlockerBeatsUnknown: with both a False and an Unknown
// dependency, the decision reports the DEFINITE blocker, which is the more
// actionable of the two.
func TestEvaluate_EstablishedBlockerBeatsUnknown(t *testing.T) {
	d := Evaluate(Observation{
		Found: true, RecordID: "bead-a",
		Dependencies: []Dependency{dep("bead-ghost", ConditionUnknown), dep("bead-b", ConditionFalse)},
	})

	if d.Admitted {
		t.Fatalf("must not admit, got %+v", d)
	}
	if d.Reason != ReasonWaitingForDependency {
		t.Fatalf("False must win over Unknown, got reason %s", d.Reason)
	}
	if !reflect.DeepEqual(d.Blockers, []string{"bead-b"}) {
		t.Fatalf("blockers should name the established one, got %v", d.Blockers)
	}
}

// TestEvaluate_DegradedNeverAdmits is the no-false-satisfaction rule for an
// unavailable observer: "I could not read authoritatively" must never be
// rendered as "nothing to wait for".
func TestEvaluate_DegradedNeverAdmits(t *testing.T) {
	d := Evaluate(Observation{
		Subject:  Subject{Repo: "acme/widget", Number: 7},
		Degraded: true, DegradedReason: "LedgerPartial",
	})

	if d.Admitted {
		t.Fatalf("a degraded observation must never admit, got %+v", d)
	}
	if d.Reason != "LedgerPartial" {
		t.Fatalf("degraded reason should surface, got %s", d.Reason)
	}
	for _, c := range d.Conditions {
		if c.Status != ConditionUnknown {
			t.Fatalf("degraded conditions should all be Unknown, got %s=%s", c.Type, c.Status)
		}
	}
}

// TestEvaluate_DegradedWithoutReasonHasFallback keeps log lines and operator
// messages from going blank when an observer forgets to name its failure.
func TestEvaluate_DegradedWithoutReasonHasFallback(t *testing.T) {
	d := Evaluate(Observation{Degraded: true})
	if d.Admitted || d.Reason == "" {
		t.Fatalf("degraded-with-no-reason should block with a fallback reason, got %+v", d)
	}
}

// TestEvaluate_IsPureAndOrderIndependent: the decision is a function of current
// observed state alone. Evaluating the same observation repeatedly, and
// evaluating observations in any order, yields the same answers — which is why
// duplicate or out-of-order events cannot wedge admission.
func TestEvaluate_IsPureAndOrderIndependent(t *testing.T) {
	blocked := Observation{Found: true, RecordID: "a", Dependencies: []Dependency{dep("b", ConditionFalse)}}
	open := Observation{Found: true, RecordID: "c"}

	first := Evaluate(blocked)
	// Interleave an unrelated evaluation, then repeat the first twice more.
	_ = Evaluate(open)
	second := Evaluate(blocked)
	_ = Evaluate(open)
	third := Evaluate(blocked)

	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(second, third) {
		t.Fatalf("Evaluate must be pure: %+v vs %+v vs %+v", first, second, third)
	}
	if Evaluate(blocked).Admitted {
		t.Fatal("blocked observation must stay blocked no matter how often it is evaluated")
	}
	if !Evaluate(open).Admitted {
		t.Fatal("an unrelated ready observation must stay admitted regardless of the blocked one")
	}
}

// TestEvaluate_BlockersAreSorted keeps logs and test assertions deterministic
// when a candidate has several blockers.
func TestEvaluate_BlockersAreSorted(t *testing.T) {
	d := Evaluate(Observation{
		Found: true, RecordID: "a",
		Dependencies: []Dependency{dep("zeta", ConditionFalse), dep("alpha", ConditionFalse)},
	})
	if !reflect.DeepEqual(d.Blockers, []string{"alpha", "zeta"}) {
		t.Fatalf("blockers should be sorted, got %v", d.Blockers)
	}
}

func TestSubjectKey(t *testing.T) {
	if got := (Subject{Repo: "acme/widget", Number: 7}).Key(); got != "acme/widget#7" {
		t.Fatalf("Subject.Key() = %q, want the canonical owner/repo#number form", got)
	}
	if got := (Subject{WorkKey: "acme/widget!ENG-7"}).Key(); got != "acme/widget!ENG-7" {
		t.Fatalf("external Subject.Key() = %q, want worksource.Ref key", got)
	}
}

func TestDecisionConditionMiss(t *testing.T) {
	if _, ok := (Decision{}).Condition(ConditionReady); ok {
		t.Fatal("Condition() must report a miss on a decision with no conditions")
	}
}
